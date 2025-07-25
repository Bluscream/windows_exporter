// SPDX-License-Identifier: Apache-2.0
//
// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package logical_disk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-ole/go-ole"
	"github.com/prometheus-community/windows_exporter/internal/headers/propsys"
	"github.com/prometheus-community/windows_exporter/internal/headers/shell32"
	"github.com/prometheus-community/windows_exporter/internal/mi"
	"github.com/prometheus-community/windows_exporter/internal/pdh"
	"github.com/prometheus-community/windows_exporter/internal/types"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/windows"
)

const (
	Name                  = "logical_disk"
	subCollectorMetrics   = "metrics"
	subCollectorBitlocker = "bitlocker_status"
)

type Config struct {
	CollectorsEnabled []string       `yaml:"enabled"`
	VolumeInclude     *regexp.Regexp `yaml:"volume-include"`
	VolumeExclude     *regexp.Regexp `yaml:"volume-exclude"`
}

//nolint:gochecknoglobals
var ConfigDefaults = Config{
	CollectorsEnabled: []string{
		subCollectorMetrics,
	},
	VolumeInclude: types.RegExpAny,
	VolumeExclude: types.RegExpEmpty,
}

// A Collector is a Prometheus Collector for perflib logicalDisk metrics.
type Collector struct {
	config Config
	logger *slog.Logger

	perfDataCollector *pdh.Collector
	perfDataObject    []perfDataCounterValues

	bitlockerReqCh chan string
	bitlockerResCh chan struct {
		err    error
		status int
	}

	ctxCancelFunc context.CancelFunc

	avgReadQueue     *prometheus.Desc
	avgWriteQueue    *prometheus.Desc
	freeSpace        *prometheus.Desc
	idleTime         *prometheus.Desc
	information      *prometheus.Desc
	readBytesTotal   *prometheus.Desc
	readLatency      *prometheus.Desc
	readOnly         *prometheus.Desc
	readsTotal       *prometheus.Desc
	readTime         *prometheus.Desc
	readWriteLatency *prometheus.Desc
	requestsQueued   *prometheus.Desc
	splitIOs         *prometheus.Desc
	totalSpace       *prometheus.Desc
	writeBytesTotal  *prometheus.Desc
	writeLatency     *prometheus.Desc
	writesTotal      *prometheus.Desc
	writeTime        *prometheus.Desc

	bitlockerStatus *prometheus.Desc
}

type volumeInfo struct {
	diskIDs      string
	filesystem   string
	serialNumber string
	label        string
	volumeType   string
	readonly     float64
}

func New(config *Config) *Collector {
	if config == nil {
		config = &ConfigDefaults
	}

	if config.VolumeExclude == nil {
		config.VolumeExclude = ConfigDefaults.VolumeExclude
	}

	if config.VolumeInclude == nil {
		config.VolumeInclude = ConfigDefaults.VolumeInclude
	}

	c := &Collector{
		config: *config,
	}

	return c
}

func NewWithFlags(app *kingpin.Application) *Collector {
	c := &Collector{
		config: ConfigDefaults,
	}
	c.config.CollectorsEnabled = make([]string, 0)

	var collectorsEnabled, volumeExclude, volumeInclude string

	app.Flag(
		"collector.logical_disk.volume-exclude",
		"Regexp of volumes to exclude. Volume name must both match include and not match exclude to be included.",
	).Default("").StringVar(&volumeExclude)

	app.Flag(
		"collector.logical_disk.volume-include",
		"Regexp of volumes to include. Volume name must both match include and not match exclude to be included.",
	).Default(".+").StringVar(&volumeInclude)

	app.Flag(
		"collector.logical_disk.enabled",
		fmt.Sprintf("Comma-separated list of collectors to use. Available collectors: %s, %s. Defaults to metrics, if not specified.",
			subCollectorMetrics,
			subCollectorBitlocker,
		),
	).Default(strings.Join(ConfigDefaults.CollectorsEnabled, ",")).StringVar(&collectorsEnabled)

	app.Action(func(*kingpin.ParseContext) error {
		c.config.CollectorsEnabled = strings.Split(collectorsEnabled, ",")

		var err error

		c.config.VolumeExclude, err = regexp.Compile(fmt.Sprintf("^(?:%s)$", volumeExclude))
		if err != nil {
			return fmt.Errorf("collector.logical_disk.volume-exclude: %w", err)
		}

		c.config.VolumeInclude, err = regexp.Compile(fmt.Sprintf("^(?:%s)$", volumeInclude))
		if err != nil {
			return fmt.Errorf("collector.logical_disk.volume-include: %w", err)
		}

		return nil
	})

	return c
}

func (c *Collector) GetName() string {
	return Name
}

func (c *Collector) Close() error {
	if slices.Contains(c.config.CollectorsEnabled, subCollectorBitlocker) {
		c.ctxCancelFunc()
	}

	return nil
}

func (c *Collector) Build(logger *slog.Logger, _ *mi.Session) error {
	c.logger = logger.With(slog.String("collector", Name))

	for _, collector := range c.config.CollectorsEnabled {
		if !slices.Contains([]string{subCollectorMetrics, subCollectorBitlocker}, collector) {
			return fmt.Errorf("unknown sub collector: %s. Possible values: %s", collector,
				strings.Join([]string{subCollectorMetrics, subCollectorBitlocker}, ", "),
			)
		}
	}

	c.information = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "info"),
		"A metric with a constant '1' value labeled with logical disk information",
		[]string{"disk", "type", "volume", "volume_name", "filesystem", "serial_number"},
		nil,
	)
	c.readOnly = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "readonly"),
		"Whether the logical disk is read-only",
		[]string{"volume"},
		nil,
	)
	c.requestsQueued = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "requests_queued"),
		"The number of requests queued to the disk (LogicalDisk.CurrentDiskQueueLength)",
		[]string{"volume"},
		nil,
	)

	c.avgReadQueue = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "avg_read_requests_queued"),
		"Average number of read requests that were queued for the selected disk during the sample interval (LogicalDisk.AvgDiskReadQueueLength)",
		[]string{"volume"},
		nil,
	)

	c.avgWriteQueue = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "avg_write_requests_queued"),
		"Average number of write requests that were queued for the selected disk during the sample interval (LogicalDisk.AvgDiskWriteQueueLength)",
		[]string{"volume"},
		nil,
	)

	c.readBytesTotal = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "read_bytes_total"),
		"The number of bytes transferred from the disk during read operations (LogicalDisk.DiskReadBytesPerSec)",
		[]string{"volume"},
		nil,
	)

	c.readsTotal = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "reads_total"),
		"The number of read operations on the disk (LogicalDisk.DiskReadsPerSec)",
		[]string{"volume"},
		nil,
	)

	c.writeBytesTotal = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "write_bytes_total"),
		"The number of bytes transferred to the disk during write operations (LogicalDisk.DiskWriteBytesPerSec)",
		[]string{"volume"},
		nil,
	)

	c.writesTotal = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "writes_total"),
		"The number of write operations on the disk (LogicalDisk.DiskWritesPerSec)",
		[]string{"volume"},
		nil,
	)

	c.readTime = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "read_seconds_total"),
		"Seconds that the disk was busy servicing read requests (LogicalDisk.PercentDiskReadTime)",
		[]string{"volume"},
		nil,
	)

	c.writeTime = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "write_seconds_total"),
		"Seconds that the disk was busy servicing write requests (LogicalDisk.PercentDiskWriteTime)",
		[]string{"volume"},
		nil,
	)

	c.freeSpace = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "free_bytes"),
		"Free space in bytes, updates every 10-15 min (LogicalDisk.PercentFreeSpace)",
		[]string{"volume"},
		nil,
	)

	c.totalSpace = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "size_bytes"),
		"Total space in bytes, updates every 10-15 min (LogicalDisk.PercentFreeSpace_Base)",
		[]string{"volume"},
		nil,
	)

	c.idleTime = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "idle_seconds_total"),
		"Seconds that the disk was idle (LogicalDisk.PercentIdleTime)",
		[]string{"volume"},
		nil,
	)

	c.splitIOs = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "split_ios_total"),
		"The number of I/Os to the disk were split into multiple I/Os (LogicalDisk.SplitIOPerSec)",
		[]string{"volume"},
		nil,
	)

	c.readLatency = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "read_latency_seconds_total"),
		"Shows the average time, in seconds, of a read operation from the disk (LogicalDisk.AvgDiskSecPerRead)",
		[]string{"volume"},
		nil,
	)

	c.writeLatency = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "write_latency_seconds_total"),
		"Shows the average time, in seconds, of a write operation to the disk (LogicalDisk.AvgDiskSecPerWrite)",
		[]string{"volume"},
		nil,
	)

	c.readWriteLatency = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "read_write_latency_seconds_total"),
		"Shows the time, in seconds, of the average disk transfer (LogicalDisk.AvgDiskSecPerTransfer)",
		[]string{"volume"},
		nil,
	)

	c.bitlockerStatus = prometheus.NewDesc(
		prometheus.BuildFQName(types.Namespace, Name, "bitlocker_status"),
		"BitLocker status for the logical disk",
		[]string{"volume", "status"},
		nil,
	)

	var err error

	c.perfDataCollector, err = pdh.NewCollector[perfDataCounterValues](pdh.CounterTypeRaw, "LogicalDisk", pdh.InstancesAll)
	if err != nil {
		return fmt.Errorf("failed to create LogicalDisk collector: %w", err)
	}

	if slices.Contains(c.config.CollectorsEnabled, subCollectorBitlocker) {
		initErrCh := make(chan error)
		c.bitlockerReqCh = make(chan string, 1)
		c.bitlockerResCh = make(chan struct {
			err    error
			status int
		}, 1)

		ctx, cancel := context.WithCancel(context.Background())

		c.ctxCancelFunc = cancel

		go c.workerBitlocker(ctx, initErrCh)

		if err = <-initErrCh; err != nil {
			return fmt.Errorf("failed to initialize BitLocker worker: %w", err)
		}
	}

	return nil
}

// Collect sends the metric values for each metric
// to the provided prometheus Metric channel.
func (c *Collector) Collect(ch chan<- prometheus.Metric) error {
	var info volumeInfo

	err := c.perfDataCollector.Collect(&c.perfDataObject)
	if err != nil {
		return fmt.Errorf("failed to collect LogicalDisk metrics: %w", err)
	}

	volumes, err := getAllMountedVolumes()
	if err != nil {
		return fmt.Errorf("failed to get volumes: %w", err)
	}

	for _, data := range c.perfDataObject {
		if c.config.VolumeExclude.MatchString(data.Name) || !c.config.VolumeInclude.MatchString(data.Name) {
			continue
		}

		info, err = getVolumeInfo(volumes, data.Name)
		if err != nil {
			c.logger.Warn("failed to get volume information for "+data.Name,
				slog.Any("err", err),
			)
		}

		ch <- prometheus.MustNewConstMetric(
			c.information,
			prometheus.GaugeValue,
			1,
			info.diskIDs,
			info.volumeType,
			data.Name,
			info.label,
			info.filesystem,
			info.serialNumber,
		)

		if slices.Contains(c.config.CollectorsEnabled, subCollectorMetrics) {
			ch <- prometheus.MustNewConstMetric(
				c.requestsQueued,
				prometheus.GaugeValue,
				data.CurrentDiskQueueLength,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.avgReadQueue,
				prometheus.GaugeValue,
				data.AvgDiskReadQueueLength*pdh.TicksToSecondScaleFactor,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.avgWriteQueue,
				prometheus.GaugeValue,
				data.AvgDiskWriteQueueLength*pdh.TicksToSecondScaleFactor,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.readBytesTotal,
				prometheus.CounterValue,
				data.DiskReadBytesPerSec,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.readsTotal,
				prometheus.CounterValue,
				data.DiskReadsPerSec,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.writeBytesTotal,
				prometheus.CounterValue,
				data.DiskWriteBytesPerSec,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.writesTotal,
				prometheus.CounterValue,
				data.DiskWritesPerSec,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.readTime,
				prometheus.CounterValue,
				data.PercentDiskReadTime,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.writeTime,
				prometheus.CounterValue,
				data.PercentDiskWriteTime,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.freeSpace,
				prometheus.GaugeValue,
				data.FreeSpace*1024*1024,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.totalSpace,
				prometheus.GaugeValue,
				data.PercentFreeSpace*1024*1024,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.idleTime,
				prometheus.CounterValue,
				data.PercentIdleTime,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.splitIOs,
				prometheus.CounterValue,
				data.SplitIOPerSec,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.readLatency,
				prometheus.CounterValue,
				data.AvgDiskSecPerRead*pdh.TicksToSecondScaleFactor,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.writeLatency,
				prometheus.CounterValue,
				data.AvgDiskSecPerWrite*pdh.TicksToSecondScaleFactor,
				data.Name,
			)

			ch <- prometheus.MustNewConstMetric(
				c.readWriteLatency,
				prometheus.CounterValue,
				data.AvgDiskSecPerTransfer*pdh.TicksToSecondScaleFactor,
				data.Name,
			)
		}

		if slices.Contains(c.config.CollectorsEnabled, subCollectorBitlocker) {
			c.bitlockerReqCh <- data.Name

			bitlockerStatus := <-c.bitlockerResCh

			if bitlockerStatus.err != nil {
				c.logger.Warn("failed to get BitLocker status for "+data.Name,
					slog.Any("err", bitlockerStatus.err),
				)

				continue
			}

			if bitlockerStatus.status == -1 {
				c.logger.Debug("BitLocker status for "+data.Name+" is unknown",
					slog.Int("status", bitlockerStatus.status),
				)

				continue
			}

			for i, status := range []string{"disabled", "on", "off", "encrypting", "decrypting", "suspended", "locked", "unknown", "waiting_for_activation"} {
				val := 0.0
				if bitlockerStatus.status == i {
					val = 1.0
				}

				ch <- prometheus.MustNewConstMetric(
					c.bitlockerStatus,
					prometheus.GaugeValue,
					val,
					data.Name,
					status,
				)
			}
		}
	}

	return nil
}

func getDriveType(driveType uint32) string {
	switch driveType {
	case windows.DRIVE_UNKNOWN:
		return "unknown"
	case windows.DRIVE_NO_ROOT_DIR:
		return "norootdir"
	case windows.DRIVE_REMOVABLE:
		return "removable"
	case windows.DRIVE_FIXED:
		return "fixed"
	case windows.DRIVE_REMOTE:
		return "remote"
	case windows.DRIVE_CDROM:
		return "cdrom"
	case windows.DRIVE_RAMDISK:
		return "ramdisk"
	default:
		return "unknown"
	}
}

// diskExtentSize Size of the DiskExtent structure in bytes.
const diskExtentSize = 24

// getDiskIDByVolume returns the disk ID for a given volume.
func getVolumeInfo(volumes map[string]string, rootDrive string) (volumeInfo, error) {
	volumePath := rootDrive

	// If rootDrive is a NTFS directory, convert it to a volume GUID.
	if volumeGUID, ok := volumes[rootDrive]; ok {
		// GetVolumeNameForVolumeMountPoint returns the volume GUID path as \\?\Volume{GUID}\
		// According https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-deviceiocontrol#remarks
		// Win32 Drive Namespace is prefixed with \\.\, so we need to remove the \\?\ prefix.
		volumePath, _ = strings.CutPrefix(volumeGUID, `\\?\`)
	}

	volumePathPtr := windows.StringToUTF16Ptr(`\\.\` + volumePath)

	// mode has to include FILE_SHARE permission to allow concurrent access to the disk.
	// use 0 as access mode to avoid admin permission.
	mode := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	attr := uint32(windows.FILE_ATTRIBUTE_READONLY)

	volumeHandle, err := windows.CreateFile(volumePathPtr, 0, mode, nil, windows.OPEN_EXISTING, attr, 0)
	if err != nil {
		return volumeInfo{}, fmt.Errorf("could not open volume for %s: %w", rootDrive, err)
	}

	defer func(fd windows.Handle) {
		_ = windows.Close(fd)
	}(volumeHandle)

	controlCode := uint32(5636096) // IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS
	volumeDiskExtents := make([]byte, 16*1024)

	var bytesReturned uint32

	err = windows.DeviceIoControl(volumeHandle, controlCode, nil, 0, &volumeDiskExtents[0], uint32(len(volumeDiskExtents)), &bytesReturned, nil)
	if err != nil {
		return volumeInfo{}, fmt.Errorf("could not identify physical drive for %s: %w", rootDrive, err)
	}

	numDiskIDs := uint(binary.LittleEndian.Uint32(volumeDiskExtents))
	if numDiskIDs < 1 {
		return volumeInfo{}, fmt.Errorf("could not identify physical drive for %s: no disk IDs returned", rootDrive)
	}

	diskIDs := make([]string, numDiskIDs)

	for i := range numDiskIDs {
		diskIDs[i] = strconv.FormatUint(uint64(binary.LittleEndian.Uint32(volumeDiskExtents[8+i*diskExtentSize:])), 10)
	}

	slices.Sort(diskIDs)
	diskIDs = slices.Compact(diskIDs)

	volumeInformationRootDrive := volumePath + `\`

	if strings.Contains(volumePath, `Volume`) {
		volumeInformationRootDrive = `\\?\` + volumeInformationRootDrive
	}

	volumeInformationRootDrivePtr := windows.StringToUTF16Ptr(volumeInformationRootDrive)
	driveType := windows.GetDriveType(volumeInformationRootDrivePtr)
	volBufLabel := make([]uint16, windows.MAX_PATH+1)
	volSerialNum := uint32(0)
	fsFlags := uint32(0)
	volBufType := make([]uint16, windows.MAX_PATH+1)

	err = windows.GetVolumeInformation(
		volumeInformationRootDrivePtr,
		&volBufLabel[0], uint32(len(volBufLabel)),
		&volSerialNum, nil, &fsFlags,
		&volBufType[0], uint32(len(volBufType)),
	)
	if err != nil {
		if driveType == windows.DRIVE_CDROM || driveType == windows.DRIVE_REMOVABLE {
			return volumeInfo{}, nil
		}

		return volumeInfo{}, fmt.Errorf("could not get volume information for %s: %w", volumeInformationRootDrive, err)
	}

	return volumeInfo{
		diskIDs:      strings.Join(diskIDs, ";"),
		volumeType:   getDriveType(driveType),
		label:        windows.UTF16PtrToString(&volBufLabel[0]),
		filesystem:   windows.UTF16PtrToString(&volBufType[0]),
		serialNumber: fmt.Sprintf("%X", volSerialNum),
		readonly:     float64(fsFlags & windows.FILE_READ_ONLY_VOLUME),
	}, nil
}

func getAllMountedVolumes() (map[string]string, error) {
	guidBuf := make([]uint16, windows.MAX_PATH+1)
	guidBufLen := uint32(len(guidBuf) * 2)

	hFindVolume, err := windows.FindFirstVolume(&guidBuf[0], guidBufLen)
	if err != nil {
		return nil, fmt.Errorf("FindFirstVolume: %w", err)
	}

	defer func() {
		_ = windows.FindVolumeClose(hFindVolume)
	}()

	volumes := map[string]string{}

	for ; ; err = windows.FindNextVolume(hFindVolume, &guidBuf[0], guidBufLen) {
		if err != nil {
			switch {
			case errors.Is(err, windows.ERROR_NO_MORE_FILES):
				return volumes, nil
			default:
				return nil, fmt.Errorf("FindNextVolume: %w", err)
			}
		}

		var rootPathLen uint32

		rootPathBuf := make([]uint16, windows.MAX_PATH+1)
		rootPathBufLen := uint32(len(rootPathBuf) * 2)

		for {
			err = windows.GetVolumePathNamesForVolumeName(&guidBuf[0], &rootPathBuf[0], rootPathBufLen, &rootPathLen)
			if err == nil {
				break
			}

			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
				// the volume is not mounted
				break
			}

			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				rootPathBuf = make([]uint16, (rootPathLen+1)/2)

				continue
			}

			return nil, fmt.Errorf("GetVolumePathNamesForVolumeName: %w", err)
		}

		mountPoint := windows.UTF16ToString(rootPathBuf)

		// Skip unmounted volumes
		if len(mountPoint) == 0 {
			continue
		}

		volumes[strings.TrimSuffix(mountPoint, `\`)] = strings.TrimSuffix(windows.UTF16ToString(guidBuf), `\`)
	}
}

/*
++ References

| System.Volume.      | Control Panel                    | manage-bde conversion     | manage-bde     | Get-BitlockerVolume          | Get-BitlockerVolume |
| BitLockerProtection |                                  |                           | protection     | VolumeStatus                 | ProtectionStatus    |
| ------------------- | -------------------------------- | ------------------------- | -------------- | ---------------------------- | ------------------- |
|                   1 | BitLocker on                     | Used Space Only Encrypted | Protection On  | FullyEncrypted               | On                  |
|                   1 | BitLocker on                     | Fully Encrypted           | Protection On  | FullyEncrypted               | On                  |
|                   1 | BitLocker on                     | Fully Encrypted           | Protection On  | FullyEncryptedWipeInProgress | On                  |
|                   2 | BitLocker off                    | Fully Decrypted           | Protection Off | FullyDecrypted               | Off                 |
|                   3 | BitLocker Encrypting             | Encryption In Progress    | Protection Off | EncryptionInProgress         | Off                 |
|                   3 | BitLocker Encryption Paused      | Encryption Paused         | Protection Off | EncryptionSuspended          | Off                 |
|                   4 | BitLocker Decrypting             | Decryption in progress    | Protection Off | DecyptionInProgress          | Off                 |
|                   4 | BitLocker Decryption Paused      | Decryption Paused         | Protection Off | DecryptionSuspended          | Off                 |
|                   5 | BitLocker suspended              | Used Space Only Encrypted | Protection Off | FullyEncrypted               | Off                 |
|                   5 | BitLocker suspended              | Fully Encrypted           | Protection Off | FullyEncrypted               | Off                 |
|                   6 | BitLocker on (Locked)            | Unknown                   | Unknown        | $null                        | Unknown             |
|                   7 |                                  |                           |                |                              |                     |
|                   8 | BitLocker waiting for activation | Used Space Only Encrypted | Protection Off | FullyEncrypted               | Off                 |

--
*/
func (c *Collector) workerBitlocker(ctx context.Context, initErrCh chan<- error) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("workerBitlocker panic",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)

			// Restart the workerBitlocker
			initErrCh := make(chan error)

			go c.workerBitlocker(ctx, initErrCh)

			if err := <-initErrCh; err != nil {
				c.logger.Error("workerBitlocker restart failed",
					slog.Any("err", err),
				)
			}
		}
	}()

	// The only way to run WMI queries in parallel while being thread-safe is to
	// ensure the CoInitialize[Ex]() call is bound to its current OS thread.
	// Otherwise, attempting to initialize and run parallel queries across
	// goroutines will result in protected memory errors.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED|ole.COINIT_DISABLE_OLE1DDE); err != nil {
		var oleCode *ole.OleError
		if errors.As(err, &oleCode) && oleCode.Code() != ole.S_OK && oleCode.Code() != 0x00000001 {
			initErrCh <- fmt.Errorf("CoInitializeEx: %w", err)

			return
		}
	}

	defer ole.CoUninitialize()

	var pkey propsys.PROPERTYKEY

	// The ideal solution to check the disk encryption (BitLocker) status is to
	// use the WMI APIs (Win32_EncryptableVolume). However, only programs running
	// with elevated priledges can access those APIs.
	//
	// Our alternative solution is based on the value of the undocumented (shell)
	// property: "System.Volume.BitLockerProtection". That property is essentially
	// an enum containing the current BitLocker status for a given volume. This
	// approached was suggested here:
	// https://stackoverflow.com/questions/41308245/detect-bitlocker-programmatically-from-c-sharp-without-admin/41310139
	//
	// Note that the link above doesn't give any explanation / meaning for the
	// enum values, it simply says that 1, 3 or 5 means the disk is encrypted.
	//
	// I directly tested and validated this strategy on a Windows 10 machine.
	// The values given in the BitLockerStatus enum contain the relevant values
	// for the shell property. I also directly validated them.
	if err := propsys.PSGetPropertyKeyFromName("System.Volume.BitLockerProtection", &pkey); err != nil {
		initErrCh <- fmt.Errorf("PSGetPropertyKeyFromName failed: %w", err)

		return
	}

	close(initErrCh)

	for {
		select {
		case <-ctx.Done():
			return
		case path, ok := <-c.bitlockerReqCh:
			if !ok {
				return
			}

			if !strings.Contains(path, `:`) {
				c.bitlockerResCh <- struct {
					err    error
					status int
				}{err: nil, status: -1}

				continue
			}

			status, err := func(path string) (int, error) {
				item, err := shell32.SHCreateItemFromParsingName(path)
				if err != nil {
					return -1, fmt.Errorf("SHCreateItemFromParsingName failed: %w", err)
				}

				defer item.Release()

				var v ole.VARIANT

				if err := item.GetProperty(&pkey, &v); err != nil {
					return -1, fmt.Errorf("GetProperty failed: %w", err)
				}

				return int(v.Val), v.Clear()
			}(path)

			c.bitlockerResCh <- struct {
				err    error
				status int
			}{err: err, status: status}
		}
	}
}
