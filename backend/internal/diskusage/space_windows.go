package diskusage

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func Read(path string) (Space, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Space{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Space{}, err
	}
	if !info.IsDir() {
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return Space{}, err
		}
		path = filepath.Dir(path)
	}
	dir, err := windows.UTF16PtrFromString(strings.TrimRight(path, `\`) + `\`)
	if err != nil {
		return Space{}, err
	}
	var available, total uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &available, &total, nil); err != nil {
		return Space{}, err
	}
	var mount [32768]uint16
	if err := windows.GetVolumePathName(dir, &mount[0], uint32(len(mount))); err != nil {
		return Space{}, err
	}
	volume := windows.UTF16ToString(mount[:])
	var guid [64]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(&mount[0], &guid[0], uint32(len(guid))); err == nil {
		volume = windows.UTF16ToString(guid[:])
	} else if windows.GetDriveType(&mount[0]) == windows.DRIVE_REMOTE {
		volume = shareName(path, volume)
	}
	return Space{Volume: strings.ToLower(strings.TrimRight(volume, `\`)), TotalBytes: float64(total), AvailableBytes: float64(available)}, nil
}

// SMB shares have no mount-manager GUID. Resolve mapped drive letters to a
// share when the server supports handle names, otherwise retain its mount root.
func shareName(path, mount string) string {
	file, err := os.Open(path)
	if err != nil {
		return mount
	}
	defer file.Close()
	var buf [32768]uint16
	const fileNameOpened = 8
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buf[0], uint32(len(buf)), fileNameOpened)
	if err == nil && n > 0 && n < uint32(len(buf)) {
		name := windows.UTF16ToString(buf[:n])
		if strings.HasPrefix(strings.ToUpper(name), `\\?\UNC\`) {
			return filepath.VolumeName(`\\` + name[8:])
		}
	}
	return mount
}
