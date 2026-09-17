package msdata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

// TempPathFor returns the scratch path a writer should fill before promoting the
// finished document to dst.
//
// Keeping the scratch file next to the target makes the final rename atomic; an
// explicit temporary directory is supported for layouts where the destination
// volume is slow, read-only-ish, or monitored in a way that a growing file would
// disturb. When the scratch path lands on a different filesystem the promotion
// falls back to copy-then-rename (see Promote).
func TempPathFor(dst, tempDir string) string {
	if tempDir == "" {
		return dst + ".writing"
	}
	return filepath.Join(tempDir, filepath.Base(dst)+".writing")
}

// Promote moves a completed scratch file onto its final name.
//
// os.Rename is atomic within a filesystem. Across filesystems it fails, and the
// only honest way to keep "no partial file is ever presented as finished" is to
// copy into a scratch name inside the destination directory and rename that.
func Promote(tmp, dst string) error {
	err := os.Rename(tmp, dst)
	if err == nil {
		return nil
	}
	local := dst + ".writing"
	in, ierr := os.Open(tmp)
	if ierr != nil {
		return fmt.Errorf("%w: promoting %s to %s: %v (reopen: %v)", ErrWriteFailed, tmp, dst, err, ierr)
	}
	out, oerr := os.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if oerr != nil {
		in.Close()
		return fmt.Errorf("%w: promoting %s to %s: %v (create: %v)", ErrWriteFailed, tmp, dst, err, oerr)
	}
	if _, cerr := io.Copy(out, in); cerr != nil {
		in.Close()
		out.Close()
		os.Remove(local)
		return fmt.Errorf("%w: promoting %s to %s: %v (copy: %v)", ErrWriteFailed, tmp, dst, err, cerr)
	}
	in.Close()
	if serr := out.Sync(); serr != nil {
		out.Close()
		os.Remove(local)
		return fmt.Errorf("%w: syncing %s: %v", ErrWriteFailed, dst, serr)
	}
	if cerr := out.Close(); cerr != nil {
		os.Remove(local)
		return fmt.Errorf("%w: closing %s: %v", ErrWriteFailed, dst, cerr)
	}
	if rerr := os.Rename(local, dst); rerr != nil {
		os.Remove(local)
		return fmt.Errorf("%w: renaming %s to %s: %v", ErrWriteFailed, local, dst, rerr)
	}
	// The scratch copy on the other volume is now redundant.
	os.Remove(tmp)
	return nil
}

// HashWriter counts and hashes everything a writer emits, so the digest of a
// produced document costs no extra pass over the file.
type HashWriter struct {
	w io.Writer
	h hash.Hash
	// Bytes counts the bytes handed to the underlying writer.
	Bytes int64
}

// NewHashWriter wraps w and records both a byte count and a SHA-256 digest.
func NewHashWriter(w io.Writer) *HashWriter {
	return &HashWriter{w: w, h: sha256.New()}
}

func (h *HashWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	if n > 0 {
		h.Bytes += int64(n)
		// A hash never fails; ignoring the error keeps the writer path allocation-free.
		_, _ = h.h.Write(p[:n])
	}
	return n, err
}

// HexDigest returns the hex SHA-256 of everything written so far.
func (h *HashWriter) HexDigest() string {
	if h == nil {
		return ""
	}
	return hex.EncodeToString(h.h.Sum(nil))
}
