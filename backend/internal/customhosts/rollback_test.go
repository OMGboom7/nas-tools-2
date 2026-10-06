package customhosts

import (
	"errors"
	"io"
	"testing"
)

type failedDisk struct {
	data     []byte
	position int
	failure  string
	err      error
}

func (disk *failedDisk) Seek(offset int64, origin int) (int64, error) {
	if origin == io.SeekStart {
		disk.position = int(offset)
	}
	return int64(disk.position), nil
}
func (disk *failedDisk) Write(input []byte) (int, error) {
	count := len(input)
	fail := disk.failure == "write"
	if fail {
		count = 2
		disk.failure = ""
	}
	end := disk.position + count
	if end > len(disk.data) {
		disk.data = append(disk.data, make([]byte, end-len(disk.data))...)
	}
	copy(disk.data[disk.position:end], input[:count])
	disk.position = end
	if fail {
		return count, disk.err
	}
	return count, nil
}
func (disk *failedDisk) Truncate(size int64) error {
	if disk.failure == "truncate" {
		disk.failure = ""
		return disk.err
	}
	disk.data = disk.data[:size]
	return nil
}
func (disk *failedDisk) Sync() error {
	if disk.failure == "sync" {
		disk.failure = ""
		return disk.err
	}
	return nil
}

func TestPartialWriteTruncateAndSyncFailuresRestoreOriginal(t *testing.T) {
	original := []byte("original hosts\n")
	for _, failure := range []string{"write", "truncate", "sync"} {
		fault := errors.New("disk failure")
		disk := &failedDisk{data: append([]byte(nil), original...), failure: failure, err: fault}
		if err := replaceWithRollback(t.Context(), disk, []byte("replacement hosts data\n"), original); !errors.Is(err, fault) {
			t.Fatal(failure, err)
		}
		if string(disk.data) != string(original) {
			t.Fatalf("%s left partial data: %q", failure, disk.data)
		}
	}
}
