package value

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const originKey = "config.kubernetes.io/origin"

var (
	kListKeys   = []string{"patches", "patchesStrategicMerge", "patchesJson6902", "images", "replicas", "labels", "components", "configMapGenerator", "secretGenerator"}
	kMapKeys    = []string{"commonLabels", "commonAnnotations"}
	kScalarKeys = []string{"namespace", "namePrefix", "nameSuffix"}
)

// loadStack returns the kustomization in dir and every local one it pulls in, dir first. Loading the overlay (root
// set), it copies each directory of the stack, and each file it names, from disk as it finds them: Kustomize reads no
// file outside a kustomization's own directory, so the rest of the repo stays on disk. A worker's copy (root unset)
// has the stack in memory already.
func (o *Overlay) loadStack(dir string) ([]*kfile, error) {
	var out []*kfile
	seen := map[string]bool{}
	var visit func(dir string) error
	visit = func(dir string) error {
		if seen[dir] {
			return nil
		}
		seen[dir] = true

		if o.root != "" {
			if err := o.copyDir(dir); err != nil {
				return err
			}
		}

		var kpath string
		for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
			if o.mem.Exists(path.Join(dir, n)) {
				kpath = path.Join(dir, n)
				break
			}
		}
		if kpath == "" {
			return nil
		}
		data, err := o.mem.ReadFile(kpath)
		if err != nil {
			return err
		}
		var n yaml.Node
		if err := yaml.Unmarshal(data, &n); err != nil {
			return fmt.Errorf("read %s: %w", kpath, err)
		}
		if len(n.Content) == 0 {
			return fmt.Errorf("read %s: empty kustomization", kpath)
		}
		m := n.Content[0]
		for _, key := range []string{"helmCharts", "generators", "transformers", "validators"} {
			if kget(m, key).Node != nil {
				return fmt.Errorf("%s uses %s, which kwhy doesn't run", strings.TrimPrefix(kpath, "/"), key)
			}
		}
		out = append(out, &kfile{path: kpath, doc: &n})

		for _, key := range []string{"resources", "bases", "components"} {
			for _, it := range kget(m, key).contentOrNil() {
				p := path.Join(dir, it.Value)
				if o.root != "" {
					if err := o.copyDir(p); err != nil {
						return err
					}
				}
				if o.mem.IsDir(p) {
					if err := visit(p); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}

	return out, visit(dir)
}

// copyDir copies the file or directory p, an in-memory path, from disk into memory, each file once. A path that isn't
// on disk copies nothing: render.Offline or Kustomize reports it.
func (o *Overlay) copyDir(p string) error {
	return filepath.WalkDir(filepath.Join(o.root, filepath.FromSlash(p)), func(dp string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(o.root, dp)
		if err != nil {
			return err
		}

		mp := "/" + filepath.ToSlash(rel)
		if _, ok := o.orig[mp]; ok {
			return nil
		}

		data, err := os.ReadFile(dp)
		if err != nil {
			return err
		}
		if o.mem.WriteFile(mp, data) != nil {
			return nil // a name the in-memory filesystem refuses ("..file.txt"); Kustomize couldn't read it either
		}
		o.orig[mp] = data

		return nil
	})
}

func (o *Overlay) write(kf *kfile) error {
	data, err := yaml.Marshal(kf.doc)
	if err != nil {
		return err
	}

	return o.mem.WriteFile(kf.path, data)
}

func (o *Overlay) build() (map[KField]any, []KDoc, error) {
	rm, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(o.mem, o.target)
	if err != nil {
		return nil, nil, err
	}

	return flattenAll(rm)
}

type ynode struct{ *yaml.Node }

func (n ynode) contentOrNil() []*yaml.Node {
	if n.Node == nil {
		return nil
	}

	return n.Content
}

// copyMem copies every file of one in-memory filesystem into another.
func copyMem(from, to filesys.FileSystem) error {
	return from.Walk("/", func(p string, info fs.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := from.ReadFile(p)
		if err != nil {
			return err
		}
		return to.WriteFile(p, data)
	})
}

func kget(m *yaml.Node, key string) ynode {
	if m == nil || m.Kind != yaml.MappingNode {
		return ynode{}
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return ynode{m.Content[i+1]}
		}
	}

	return ynode{}
}

func findEntries(kf *kfile) []*kentry {
	var out []*kentry
	m := kf.doc.Content[0]
	dir := path.Dir(kf.path)
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i].Value, m.Content[i+1]
		switch {
		case slices.Contains(kScalarKeys, key):
			out = append(out, &kentry{kf: kf, parent: m, index: i, isMap: true, line: val.Line, label: key})
		case slices.Contains(kMapKeys, key):
			for j := 0; j+1 < len(val.Content); j += 2 {
				out = append(out, &kentry{kf: kf, parent: val, index: j, isMap: true, line: val.Content[j].Line, label: key + "." + val.Content[j].Value})
			}
		case slices.Contains(kListKeys, key):
			for j, it := range val.Content {
				e := &kentry{kf: kf, parent: val, index: j, line: it.Line, label: fmt.Sprintf("%s[%d]", key, j)}
				if it.Kind == yaml.ScalarNode {
					e.label += " " + it.Value
					if key == "patchesStrategicMerge" {
						e.patch = path.Join(dir, it.Value)
					}
				}
				if p := kget(it, "path").Node; p != nil {
					e.label += " path=" + p.Value
					e.patch = path.Join(dir, p.Value)
				}
				if n := kget(it, "name").Node; n != nil {
					e.label += " name=" + n.Value
				}
				out = append(out, e)
				for k, l := range kget(it, "literals").contentOrNil() {
					out = append(out, &kentry{kf: kf, parent: kget(it, "literals").Node, index: k, line: l.Line, label: fmt.Sprintf("%s[%d].literals %s", key, j, l.Value)})
				}
			}
		}
	}

	return out
}

func cut(e *kentry) []*yaml.Node {
	n := 1
	if e.isMap {
		n = 2
	}
	removed := slices.Clone(e.parent.Content[e.index : e.index+n])
	e.parent.Content = slices.Delete(e.parent.Content, e.index, e.index+n)

	return removed
}

func restore(e *kentry, removed []*yaml.Node) {
	e.parent.Content = slices.Insert(e.parent.Content, e.index, removed...)
}

// flattenAll keys documents by kind + origin + position among those (rule 1) and lists by name (rule 2).
func flattenAll(rm resmap.ResMap) (map[KField]any, []KDoc, error) {
	out := map[KField]any{}
	var docs []KDoc
	seen := map[string]int{}
	for _, r := range rm.Resources() {
		raw := r.GetAnnotations()[originKey]
		k := r.GetKind() + " " + strings.ReplaceAll(strings.TrimSpace(raw), "\n", " ")
		id := fmt.Sprintf("%s #%d", k, seen[k])
		seen[k]++

		obj, err := r.Map()
		if err != nil {
			return nil, nil, err
		}
		if md, ok := obj["metadata"].(map[string]any); ok {
			if a, ok := md["annotations"].(map[string]any); ok {
				delete(a, originKey)
				if len(a) == 0 {
					delete(md, "annotations")
				}
			}
		}

		d := KDoc{ID: id, Kind: r.GetKind(), Name: r.GetName(), Namespace: r.GetNamespace(), Object: obj, ordinal: seen[k] - 1}
		if org, _ := r.GetOrigin(); org != nil {
			d.origin, d.configured = org.Path, org.ConfiguredIn
		}
		docs = append(docs, d)
		kflatten(obj, "", func(p string, v any) { out[KField{id, p}] = v })
	}

	return out, docs, nil
}

func kflatten(n any, p string, fn func(string, any)) {
	switch t := n.(type) {
	case map[string]any:
		for k, v := range t {
			kflatten(v, p+keyStep(k), fn)
		}
	case []any:
		for i, v := range t {
			kflatten(v, p+itemStep(v, i), fn)
		}
	case nil:
	default:
		fn(p, t)
	}
}

// keyStep is a map key's step in an overlay field path, quoted when it holds path syntax.
func keyStep(k string) string {
	if strings.ContainsAny(k, `.[]"`) || k == "" {
		return "[" + strconv.Quote(k) + "]"
	}

	return "." + k
}

// itemStep is list item i's step in an overlay field path: by name when it has one (rule 2), else by index.
func itemStep(item any, i int) string {
	if m, ok := item.(map[string]any); ok {
		if n, ok := m["name"].(string); ok && !strings.ContainsAny(n, "[]") {
			return "[name=" + n + "]"
		}
	}

	return "[" + strconv.Itoa(i) + "]"
}
