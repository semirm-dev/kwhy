package value

import (
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semirm-dev/kwhy/render"
)

// maxAge is how long a cache file is kept. Every new build of kwhy or of the chart changes the key, so old files are
// never read again.
const maxAge = 30 * 24 * time.Hour

func init() {
	// Rendered objects hold these behind any.
	gob.Register(map[string]any{})
	gob.Register([]any{})
}

// Explain is explain, read from the trace's cache when it holds this field's answer.
func (idx *Index) Explain(ctx context.Context, f Field) (Answer, error) {
	if idx.cacheKey == "" {
		return idx.explain(ctx, f)
	}

	sum := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s", f.Doc, f.Path))
	path := filepath.Join(idx.cacheDir, idx.cacheKey+"-"+hex.EncodeToString(sum[:8])+".gob")

	var a Answer
	if read(path, &a) == nil {
		return a, nil
	}

	a, err := idx.explain(ctx, f)
	if err != nil {
		return Answer{}, err
	}

	write(idx.cacheDir, path, a)

	return a, nil
}

// cached is what a trace cache file holds: the Index's exported fields.
type cached struct {
	Docs       []render.Doc
	Fields     map[Field]any
	Hits       map[Field][]Hit
	Random     map[Field]bool
	Unmarkable map[int]string
}

// Trace is trace, read from cacheDir when it holds this trace; the Index's Explain caches answers there too.
// The key covers the chart's files, the values Helm renders with, the options, the chunk size and the kwhy binary
// itself, so any change to them computes again. cacheDir "" (or a key that couldn't be made) computes every time.
// Files that can't be read or written are ignored: a cache only ever saves time.
func Trace(ctx context.Context, c *render.Chart, v *Values, o render.Options, chunkSize int, cacheDir string) (*Index, error) {
	var key string
	if cacheDir != "" {
		if k, err := cacheKey(c, v, o, chunkSize); err == nil {
			key = k
		}
	}

	path := filepath.Join(cacheDir, key+".gob")
	var idx *Index
	if key != "" {
		idx, _ = readCache(c, v, path)
	}

	if idx == nil {
		var err error
		if idx, err = trace(ctx, c, v, o, chunkSize); err != nil {
			return nil, err
		}
		if key != "" {
			prune(cacheDir)
			write(cacheDir, path, cached{idx.Docs, idx.Fields, idx.Hits, idx.Random, idx.Unmarkable})
		}
	}

	idx.c, idx.v, idx.o, idx.cacheDir, idx.cacheKey = c, v, o, cacheDir, key

	return idx, nil
}

func cacheKey(c *render.Chart, v *Values, o render.Options, chunkSize int) (string, error) {
	digest, err := c.Digest()
	if err != nil {
		return "", err
	}

	user, err := json.Marshal(v.User) // map keys are sorted, so equal values give equal bytes
	if err != nil {
		return "", err
	}

	exe, err := binaryID()
	if err != nil {
		return "", err
	}

	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%+v\x00%d\x00%s", digest, user, o, chunkSize, exe)

	return hex.EncodeToString(h.Sum(nil)), nil
}

// binaryID tells this kwhy binary from another build, so a new kwhy never reads an old one's cache.
func binaryID() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%d\x00%d", fi.Size(), fi.ModTime().UnixNano()), nil
}

func readCache(c *render.Chart, v *Values, path string) (*Index, error) {
	var cc cached
	if err := read(path, &cc); err != nil {
		return nil, err
	}

	coalesced, err := c.Coalesce(v.User)
	if err != nil {
		return nil, err
	}

	return &Index{Docs: cc.Docs, Fields: cc.Fields, Hits: cc.Hits, Random: cc.Random, Unmarkable: cc.Unmarkable, coalesced: coalesced}, nil
}

func read(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := gob.NewDecoder(f).Decode(out); err != nil {
		return err
	}
	now := time.Now()
	os.Chtimes(path, now, now) //nolint:errcheck // only keeps a file in use from being pruned

	return nil
}

// prune deletes kwhy's files in dir not used for maxAge (read refreshes the time). It runs once per computed trace
// or overlay, not per answer.
func prune(dir string) {
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		ours := filepath.Ext(e.Name()) == ".gob" || strings.HasPrefix(e.Name(), "tmp-")
		if fi, err := e.Info(); err == nil && ours && fi.Mode().IsRegular() && time.Since(fi.ModTime()) > maxAge {
			os.Remove(filepath.Join(dir, e.Name())) //nolint:errcheck // a file left behind is tried again next time
		}
	}
}

// write stores v at path through a temp file, so a reader never sees half a file.
func write(dir, path string, v any) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	tmp, err := os.CreateTemp(dir, "tmp-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())

	err = gob.NewEncoder(tmp).Encode(v)
	if cerr := tmp.Close(); err == nil && cerr == nil {
		os.Rename(tmp.Name(), path) //nolint:errcheck // a file that wasn't written is computed again next time
	}
}
