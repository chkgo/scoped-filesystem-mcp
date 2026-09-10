package platform

import (
	"golang.org/x/sys/unix"
	"os"
	"time"
)

func metadata(stat unix.Stat_t) Metadata {
	return Metadata{Mode: unixMode(stat.Mode), Size: stat.Size, ModifiedAt: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC(), Identity: unixIdentity(stat.Dev, stat.Ino)}
}
func birthTimeAt(parent *os.File, name string, flags int, result *Metadata) {
	var stat unix.Statx_t
	if err := unix.Statx(int(parent.Fd()), name, flags, unix.STATX_BASIC_STATS|unix.STATX_BTIME, &stat); err != nil || stat.Mask&unix.STATX_BTIME == 0 {
		return
	}
	id := unixIdentity(unix.Mkdev(stat.Dev_major, stat.Dev_minor), stat.Ino)
	if id != result.Identity {
		return
	}
	birth := time.Unix(stat.Btime.Sec, int64(stat.Btime.Nsec)).UTC()
	result.CreatedAt = &birth
}
func LstatAt(parent *os.File, name string) (Metadata, error) {
	if err := validName(name); err != nil {
		return Metadata{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return Metadata{}, normalizeError(err)
	}
	result := metadata(stat)
	birthTimeAt(parent, name, unix.AT_SYMLINK_NOFOLLOW, &result)
	return result, nil
}
func MetadataForFile(file *os.File) (Metadata, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return Metadata{}, normalizeError(err)
	}
	result := metadata(stat)
	birthTimeAt(file, "", unix.AT_EMPTY_PATH, &result)
	return result, nil
}
