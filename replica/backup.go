package replica

import (
	"fmt"
	"os"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/longhorn/backupstore"
)

const (
	snapBlockSize = 2 << 20 // 2MiB
)

/*
type DeltaBlockBackupOperations interface {
	HasSnapshot(id, volumeID string) bool
	CompareSnapshot(id, compareID, volumeID string) (*metadata.Mappings, error)
	OpenSnapshot(id, volumeID string) error
	ReadSnapshot(id, volumeID string, start int64, data []byte) error
	CloseSnapshot(id, volumeID string) error
}
*/

type Backup struct {
	lock           sync.Mutex
	backingFile    *BackingFile
	replica        *Replica
	volumeID       string
	SnapshotID     string
	BackupError    string
	BackupProgress int
	BackupURL      string
}

func NewBackup(backingFile *BackingFile) *Backup {
	return &Backup{
		backingFile: backingFile,
	}
}

func (rb *Backup) UpdateBackupStatus(snapID, volumeID string, progress int, url string, errString string) error {
	id := GenerateSnapshotDiskName(snapID)
	rb.lock.Lock()
	defer rb.lock.Unlock()
	if err := rb.assertOpen(id, volumeID); err != nil {
		logrus.Errorf("Returning Error from UpdateBackupProgress")
		return err
	}

	rb.BackupProgress = progress
	rb.BackupURL = url
	rb.BackupError = errString

	return nil
}

func (rb *Backup) HasSnapshot(snapID, volumeID string) bool {
	rb.lock.Lock()
	defer rb.lock.Unlock()
	if rb.volumeID != volumeID {
		logrus.Warnf("Invalid state volume [%s] are open, not [%s]", rb.volumeID, volumeID)
		return false
	}
	id := GenerateSnapshotDiskName(snapID)
	to := rb.findIndex(id)
	if to < 0 {
		return false
	}
	return true
}

func (rb *Backup) OpenSnapshot(snapID, volumeID string) error {
	id := GenerateSnapshotDiskName(snapID)
	rb.lock.Lock()
	defer rb.lock.Unlock()
	if rb.volumeID == volumeID && rb.SnapshotID == id {
		return nil
	}

	if rb.volumeID != "" {
		return fmt.Errorf("Volume %s and snapshot %s are already open, close first", rb.volumeID, rb.SnapshotID)
	}

	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("Cannot get working directory: %v", err)
	}
	r, err := NewReadOnly(dir, id, rb.backingFile)
	if err != nil {
		return err
	}

	rb.replica = r
	rb.volumeID = volumeID
	rb.SnapshotID = id

	return nil
}

func (rb *Backup) assertOpen(id, volumeID string) error {
	if rb.volumeID != volumeID || rb.SnapshotID != id {
		return fmt.Errorf("Invalid state volume [%s] and snapshot [%s] are open, not volume [%s], snapshot [%s]", rb.volumeID, rb.SnapshotID, volumeID, id)
	}
	return nil
}

func (rb *Backup) ReadSnapshot(snapID, volumeID string, start int64, data []byte) error {
	id := GenerateSnapshotDiskName(snapID)
	rb.lock.Lock()
	defer rb.lock.Unlock()
	if err := rb.assertOpen(id, volumeID); err != nil {
		return err
	}
	if rb.SnapshotID != id && rb.volumeID != volumeID {
		return fmt.Errorf("Snapshot %s and volume %s are not open", id, volumeID)
	}

	_, err := rb.replica.ReadAt(data, start)
	return err
}

func (rb *Backup) CloseSnapshot(snapID, volumeID string) error {
	id := GenerateSnapshotDiskName(snapID)
	rb.lock.Lock()
	defer rb.lock.Unlock()
	if err := rb.assertOpen(id, volumeID); err != nil {
		return err
	}

	if rb.volumeID == "" {
		return nil
	}

	err := rb.replica.Close()

	rb.replica = nil
	rb.volumeID = ""
	//Keeping the SnapshotID value populated as this will be used by the engine for displaying the progress
	//associated with this snapshot.
	//Also, this serves the purpose to ensure if the snapshot file is open or not as assertOpen function will check
	//for both volumeID and SnapshotID to be ""

	//rb.snapshotID = ""

	return err
}

func (rb *Backup) CompareSnapshot(snapID, compareSnapID, volumeID string) (*backupstore.Mappings, error) {
	id := GenerateSnapshotDiskName(snapID)
	compareID := ""
	if compareSnapID != "" {
		compareID = GenerateSnapshotDiskName(compareSnapID)
	}
	rb.lock.Lock()
	if err := rb.assertOpen(id, volumeID); err != nil {
		rb.lock.Unlock()
		return nil, err
	}
	rb.lock.Unlock()

	rb.replica.Lock()
	defer rb.replica.Unlock()

	from := rb.findIndex(id)
	if from < 0 {
		return nil, fmt.Errorf("Failed to find snapshot %s in chain", id)
	}

	to := rb.findIndex(compareID)
	if to < 0 {
		return nil, fmt.Errorf("Failed to find snapshot %s in chain", compareID)
	}

	mappings := &backupstore.Mappings{
		BlockSize: snapBlockSize,
	}
	mapping := backupstore.Mapping{
		Offset: -1,
	}

	if err := preload(&rb.replica.volume); err != nil {
		return nil, err
	}

	for i, val := range rb.replica.volume.location {
		if val <= byte(from) && val > byte(to) {
			offset := int64(i) * rb.replica.volume.sectorSize
			// align
			offset -= (offset % snapBlockSize)
			if mapping.Offset != offset {
				mapping = backupstore.Mapping{
					Offset: offset,
					Size:   snapBlockSize,
				}
				mappings.Mappings = append(mappings.Mappings, mapping)
			}
		}
	}

	return mappings, nil
}

func (rb *Backup) findIndex(id string) int {
	if id == "" {
		if rb.backingFile == nil {
			return 0
		}
		return 1
	}

	for i, disk := range rb.replica.activeDiskData {
		if i == 0 {
			continue
		}
		if disk.Name == id {
			return i
		}
	}
	logrus.Warnf("Cannot find snapshot %s in activeDiskData list of volume %s", id, rb.volumeID)
	return -1
}

func preload(d *diffDisk) error {
	for i, f := range d.files {
		if i == 0 {
			continue
		}

		if i == 1 {
			// Reinitialize to zero so that we can detect holes in the base snapshot
			for j := 0; j < len(d.location); j++ {
				d.location[j] = 0
			}
		}

		generator := newGenerator(d, f)
		for offset := range generator.Generate() {
			d.location[offset] = byte(i)
		}

		if generator.Err() != nil {
			return generator.Err()
		}
	}

	return nil
}
