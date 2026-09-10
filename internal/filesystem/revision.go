package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// Revision returns the stable content revision used for conflict protection.
func Revision(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RevisionReader hashes a stream without retaining its contents and observes
// cancellation between bounded reads.
func RevisionReader(ctx context.Context, reader io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := copyWithContext(ctx, h, reader)
	if err != nil {
		return "", n, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyWithContext(ctx context.Context, dst hash.Hash, src io.Reader) (int64, error) {
	buffer := make([]byte, 64*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buffer)
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
