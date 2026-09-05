package supervise

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// checkpointStore reads and writes the small integer files under the state
// directory: resume checkpoints and pinned version numbers.
//
// A resume point is only valid for the same (indexable, post-types, version)
// triple that produced it, so the filename carries that scope. An ID reached
// while indexing one post type says nothing about a full index, and a build
// targeting version 4 must not inherit version 3's progress — either mistake
// silently skips every object above that ID and yields an index that looks
// complete.
type checkpointStore struct {
	dir       string
	postTypes string
}

func (s checkpointStore) scopeKey(indexable string, version int) string {
	parts := []string{indexable}
	if indexable == "post" && s.postTypes != "" {
		// A lossy slug aliases filters such as "news,page" and "news_page".
		// Do not inherit old slug-only checkpoints: replaying is safe, skipping isn't.
		digest := sha256.Sum256([]byte(s.postTypes))
		parts = append(parts, fmt.Sprintf("types-%x", digest[:8]))
	}
	if version > 0 {
		parts = append(parts, fmt.Sprintf("v%d", version))
	}
	return strings.Join(parts, ".")
}

func (s checkpointStore) checkpointPath(indexable string, version int) string {
	return filepath.Join(s.dir, "checkpoint."+s.scopeKey(indexable, version))
}

func (s checkpointStore) versionPath(indexable string) string {
	return filepath.Join(s.dir, "version."+s.scopeKey(indexable, 0))
}

// ReadCheckpoint returns the lowest object ID reached, or 0 if none.
func (s checkpointStore) ReadCheckpoint(indexable string, version int) int64 {
	return readPositiveInt(s.checkpointPath(indexable, version))
}

func (s checkpointStore) WriteCheckpoint(indexable string, version int, id int64) error {
	return writeIntAtomic(s.checkpointPath(indexable, version), id)
}

func (s checkpointStore) ClearCheckpoint(indexable string, version int) error {
	return removeStateFile(s.checkpointPath(indexable, version))
}

// PinnedVersion is the index version a previous run was building into, or 0.
func (s checkpointStore) PinnedVersion(indexable string) int {
	return int(readPositiveInt(s.versionPath(indexable)))
}

func (s checkpointStore) PinVersion(indexable string, version int) error {
	return writeIntAtomic(s.versionPath(indexable), int64(version))
}

func (s checkpointStore) UnpinVersion(indexable string) error {
	return removeStateFile(s.versionPath(indexable))
}

func removeStateFile(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func readPositiveInt(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// writeIntAtomic writes via temp file, fsync, rename. This tool exists
// because the process is killed at arbitrary moments: a plain truncating
// write hit by a badly timed SIGKILL leaves an empty file, which reads as "no
// checkpoint" and silently restarts the phase from the top.
func writeIntAtomic(path string, value int64) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.FormatInt(value, 10))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
