package change

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/getter"

	"github.com/semirm-dev/kwhy/render"
)

// ErrNotFound marks a chart or manifests path at neither ref: a typo or an uncommitted path, not a change.
var ErrNotFound = errors.New("not found")

// Source is what to render at each ref. Paths are as seen from the working directory; those inside
// the git repo are read from the ref's checkout, others (and URLs) as they are.
type Source struct {
	// Chart is a chart directory; Manifests a directory of plain YAML instead. Values apply to Chart only.
	Chart, Manifests string
	Values           values.Options
}

// tree is a temporary git worktree with ref checked out, of the repo at root.
type tree struct {
	ref, root, dir string
}

// Render renders targets at base and head, each read from a checkout of its ref. With apps, a directory of Argo CD
// Applications or Flux objects, each ref's own app definitions are the targets instead, and warnings say which apps
// were skipped and why. Every chart renders for kubeVersion, with helm's settings.
func Render(ctx context.Context, base, head string, targets []Target, apps, kubeVersion string,
	helm *cli.EnvSettings) (b, h []render.Doc, warnings []string, err error) {
	trees := map[string]*tree{}
	for _, ref := range []string{base, head} {
		if trees[ref] != nil {
			continue
		}

		t, done, err := checkout(ctx, ref)
		if err != nil {
			return nil, nil, nil, err
		}
		defer done()

		trees[ref] = t
	}

	at := map[string][]Target{base: targets, head: targets}

	// A path at neither ref renders nothing and would report no effect: a typo, not a change.
	if apps == "" {
		for _, t := range targets {
			if !trees[base].has(t.Source) && !trees[head].has(t.Source) {
				return nil, nil, nil, fmt.Errorf("%s: %w at %s or %s (uncommitted? commit it first)",
					cmp.Or(t.Manifests, t.Chart), ErrNotFound, base, head)
			}
		}
	}

	if apps != "" {
		tmp, err := os.MkdirTemp("", "kwhy-apps-")
		if err != nil {
			return nil, nil, nil, err
		}
		defer os.RemoveAll(tmp)

		remote := remoteURL(ctx)
		for _, ref := range []string{base, head} {
			ts, ws, err := trees[ref].discover(apps, remote, tmp)
			if err != nil {
				return nil, nil, nil, err
			}

			at[ref] = ts
			for _, w := range ws {
				if !slices.Contains(warnings, w) {
					warnings = append(warnings, w)
				}
			}
		}

		if len(at[base])+len(at[head]) == 0 {
			return nil, nil, nil, fmt.Errorf("%s: no Argo CD Application or Flux object this repo can render", apps)
		}
	}

	renderAt := func(ref string) ([]render.Doc, error) {
		var docs []render.Doc
		for _, t := range at[ref] {
			o := render.Options{Release: t.Release, Namespace: t.Namespace, KubeVersion: kubeVersion}
			ds, err := trees[ref].render(ctx, t.Source, o, helm)
			if err != nil {
				return nil, err
			}
			docs = append(docs, ds...)
		}

		return docs, nil
	}

	if b, err = renderAt(base); err != nil {
		return nil, nil, nil, err
	}
	if h, err = renderAt(head); err != nil {
		return nil, nil, nil, err
	}

	return b, h, warnings, nil
}

// checkout checks ref out into a temporary git worktree; done removes it.
func checkout(ctx context.Context, ref string) (t *tree, done func(), err error) {
	root, err := git(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil, err
	}

	tmp, err := os.MkdirTemp("", "kwhy-change-")
	if err != nil {
		return nil, nil, fmt.Errorf("checkout %s: %w", ref, err)
	}

	dir := filepath.Join(tmp, "wt")
	if _, err := git(ctx, root, "worktree", "add", "--detach", "--quiet", dir, ref); err != nil {
		os.RemoveAll(tmp)
		return nil, nil, err
	}

	done = func() {
		// Background context: the worktree must go even after Ctrl-C.
		git(context.Background(), root, "worktree", "remove", "--force", dir) //nolint:errcheck
		os.RemoveAll(tmp)
	}

	if err := linksInside(dir); err != nil {
		done()
		return nil, nil, fmt.Errorf("%s: %w", ref, err)
	}

	return &tree{ref, root, dir}, done, nil
}

// linksInside refuses a checkout with a symlink that resolves outside it: Helm and Kustomize follow links, so one
// from a pull request would render the runner's files into the report.
func linksInside(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}

	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}

		target, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil // dangling: reads nothing
		}

		if rel, err := filepath.Rel(real, target); err != nil || !filepath.IsLocal(rel) {
			name, _ := filepath.Rel(dir, p)
			return fmt.Errorf("%s links outside the repo: refused", name)
		}

		return nil
	})
}

// discover is Discover on the checkout: the apps as its ref defines them, their paths anchored at the repo root so
// they resolve wherever kwhy runs. An apps directory the ref doesn't have holds none.
func (t *tree) discover(dir, remote, tmp string) ([]Target, []string, error) {
	d := inTree(t.root, t.dir, dir)
	if _, err := os.Stat(d); errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}

	ts, warnings, err := Discover(t.dir, d, remote, tmp)
	if err != nil {
		return nil, nil, err
	}

	rooted := func(p string) string {
		if p == "" || filepath.IsAbs(p) { // absolute: an inline values file under tmp
			return p
		}
		return filepath.Join(t.root, p)
	}
	for i := range ts {
		ts[i].Chart, ts[i].Manifests = rooted(ts[i].Chart), rooted(ts[i].Manifests)
		for j, f := range ts[i].Values.ValueFiles {
			ts[i].Values.ValueFiles[j] = rooted(f)
		}
	}

	return ts, warnings, nil
}

// has says whether src's chart or manifests path exists in the checkout; a URL always does.
func (t *tree) has(src Source) bool {
	path := t.path(src)
	return exists(path) || strings.Contains(path, "://")
}

// path is src's chart or manifests path in the checkout.
func (t *tree) path(src Source) string {
	return inTree(t.root, t.dir, cmp.Or(src.Manifests, src.Chart))
}

// render renders src in the checkout.
func (t *tree) render(ctx context.Context, src Source, o render.Options, helm *cli.EnvSettings) ([]render.Doc, error) {
	// An app added or removed by the change has no path at one ref: nothing renders there.
	if !t.has(src) {
		return nil, nil
	}

	path := t.path(src)
	if src.Manifests != "" {
		read := render.ReadManifests
		if isKustomize(path) {
			read = render.Kustomize
		}

		docs, err := read(path, cmp.Or(o.Namespace, "default"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.ref, err)
		}

		return docs, nil
	}

	opts := src.Values
	opts.ValueFiles = make([]string, len(src.Values.ValueFiles))
	for i, f := range src.Values.ValueFiles {
		opts.ValueFiles[i] = inTree(t.root, t.dir, f)
	}
	opts.FileValues = make([]string, len(src.Values.FileValues))
	for i, s := range src.Values.FileValues {
		// shortcut: rewrites a single key=path; a comma-separated list is read from the working directory.
		if k, p, ok := strings.Cut(s, "="); ok && !strings.Contains(s, ",") {
			s = k + "=" + inTree(t.root, t.dir, p)
		}
		opts.FileValues[i] = s
	}

	c, err := render.LoadChart(path, "", helm)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.ref, err)
	}

	user, err := opts.MergeValues(getter.All(helm))
	if err != nil {
		return nil, fmt.Errorf("%s: merge values: %w", t.ref, err)
	}

	docs, err := c.Render(ctx, user, o)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.ref, err)
	}

	return docs, nil
}

// remoteURL is the URL of the repo's origin remote, "" if there is none.
func remoteURL(ctx context.Context) string {
	url, err := git(ctx, "", "remote", "get-url", "origin")
	if err != nil {
		return ""
	}

	return strings.TrimSpace(url)
}

// inTree maps a local path inside the repo at root to the same path in the checkout at dir.
func inTree(root, dir, p string) string {
	if strings.Contains(p, "://") {
		return p
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}

	// Resolve symlinks (macOS /var → /private/var) so the path compares with git's root.
	if real, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		abs = filepath.Join(real, filepath.Base(abs))
	}

	rel, err := filepath.Rel(root, abs)
	if err != nil || !filepath.IsLocal(rel) {
		return p
	}

	return filepath.Join(dir, rel)
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}

	return strings.TrimSpace(string(out)), nil
}
