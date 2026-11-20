package change

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/getter"

	"github.com/semirm-dev/kwhy/render"
)

// Source is what to render at each ref. Paths are as seen from the working directory; those inside
// the git repo are read from the ref's checkout, others (and URLs) as they are.
type Source struct {
	// Chart is a chart directory; Manifests a directory of plain YAML instead. Values apply to Chart only.
	Chart, Manifests string
	Values           values.Options
}

// Render checks ref out into a temporary git worktree, renders src there and removes the worktree.
func Render(ctx context.Context, ref string, src Source, o render.Options, helm *cli.EnvSettings) ([]render.Doc, error) {
	root, err := git(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp("", "kwhy-change-")
	if err != nil {
		return nil, fmt.Errorf("checkout %s: %w", ref, err)
	}
	defer os.RemoveAll(tmp)

	dir := filepath.Join(tmp, "wt")
	if _, err := git(ctx, root, "worktree", "add", "--detach", "--quiet", dir, ref); err != nil {
		return nil, err
	}
	// Background context: the worktree must go even after Ctrl-C.
	defer git(context.Background(), root, "worktree", "remove", "--force", dir) //nolint:errcheck

	if src.Manifests != "" {
		ns := o.Namespace
		if ns == "" {
			ns = "default"
		}
		docs, err := render.ReadManifests(inTree(root, dir, src.Manifests), ns)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref, err)
		}
		return docs, nil
	}

	opts := src.Values
	opts.ValueFiles = make([]string, len(src.Values.ValueFiles))
	for i, f := range src.Values.ValueFiles {
		opts.ValueFiles[i] = inTree(root, dir, f)
	}
	opts.FileValues = make([]string, len(src.Values.FileValues))
	for i, s := range src.Values.FileValues {
		// shortcut: rewrites a single key=path; a comma-separated list is read from the working directory.
		if k, p, ok := strings.Cut(s, "="); ok && !strings.Contains(s, ",") {
			s = k + "=" + inTree(root, dir, p)
		}
		opts.FileValues[i] = s
	}

	c, err := render.LoadChart(inTree(root, dir, src.Chart), "", helm)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}

	user, err := opts.MergeValues(getter.All(helm))
	if err != nil {
		return nil, fmt.Errorf("%s: merge values: %w", ref, err)
	}

	docs, err := c.Render(ctx, user, o)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}

	return docs, nil
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
	if err != nil || strings.HasPrefix(rel, "..") {
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
