package platform

import (
	"golang.org/x/sys/unix"
	"os"
	"time"
)

func metadata(stat unix.Stat_t) Metadata {
	birth := time.Unix(stat.Btim.Sec, stat.Btim.Nsec).UTC()
	return Metadata{Mode: unixMode(uint32(stat.Mode)), Size: stat.Size, ModifiedAt: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC(), CreatedAt: &birth, Identity: unixIdentity(uint64(stat.Dev), stat.Ino)}
}
func LstatAt(parent *os.File, name string) (Metadata, error) {
	if err := validName(name); err != nil {
		return Metadata{}, err
	}
	var stat unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return Metadata{}, normalizeError(err)
	}
	return metadata(stat), nil
}
func MetadataForFile(file *os.File) (Metadata, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return Metadata{}, normalizeError(err)
	}
	return metadata(stat), nil
}
