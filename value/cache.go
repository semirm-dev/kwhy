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

	"github.com/semirm-dev/kwhy/render"
)

// cached is what a trace cache file holds: the Index's exported fields.
type cached struct {
	Docs       []render.Doc
	Fields     map[Field]any
	Hits       map[Field][]Hit
	Random     map[Field]bool
	Unmarkable map[int]string
}

func init() {
	// Rendered objects hold these behind any.
	gob.Register(map[string]any{})
	gob.Register([]any{})
}

// Cache keeps traces and answers between runs. Its key covers the chart's files, the values Helm renders with,
// the options, the chunk size and the kwhy binary itself, so any change to them computes again. A zero Cache
// (no dir, or a key that couldn't be made) computes every time. Files that can't be read or written are ignored:
// a cache only ever saves time.
type Cache struct {
	dir, key string
}

// OpenCache returns the cache in dir for this chart, values and options; dir "" disables it.
func OpenCache(c *render.Chart, v *Values, o render.Options, chunkSize int, dir string) Cache {
	if dir == "" {
		return Cache{}
	}

	key, err := cacheKey(c, v, o, chunkSize)
	if err != nil {
		return Cache{}
	}

	return Cache{dir: dir, key: key}
}

// Trace is Trace, read from the cache when it holds this trace.
func (k Cache) Trace(ctx context.Context, c *render.Chart, v *Values, o render.Options, chunkSize int) (*Index, error) {
	if k.key == "" {
		return Trace(ctx, c, v, o, chunkSize)
	}

	path := filepath.Join(k.dir, k.key+".gob")
	if idx, err := readCache(c, v, path); err == nil {
		return idx, nil
	}

	idx, err := Trace(ctx, c, v, o, chunkSize)
	if err != nil {
		return nil, err
	}

	write(k.dir, path, cached{idx.Docs, idx.Fields, idx.Hits, idx.Random, idx.Unmarkable})

	return idx, nil
}

// Explain is Explain, read from the cache when it holds this field's answer.
func (k Cache) Explain(ctx context.Context, idx *Index, c *render.Chart, v *Values, o render.Options, f Field) (Answer, error) {
	if k.key == "" {
		return Explain(ctx, idx, c, v, o, f)
	}

	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", f.Doc, f.Path)))
	path := filepath.Join(k.dir, k.key+"-"+hex.EncodeToString(sum[:8])+".gob")

	var a Answer
	if read(path, &a) == nil {
		return a, nil
	}

	a, err := Explain(ctx, idx, c, v, o, f)
	if err != nil {
		return Answer{}, err
	}

	write(k.dir, path, a)

	return a, nil
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

	return gob.NewDecoder(f).Decode(out)
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
