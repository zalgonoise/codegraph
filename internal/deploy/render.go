package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Chart describes how to render an environment's Helm chart.
type Chart struct {
	Dir     string   // the chart directory
	Values  []string // values files, in order, relative to Dir unless absolute
	Secrets []string // secret values files (sops): only their keys are used
}

// Render runs `helm template` on the chart and returns the rendered objects.
//
// It never contacts a cluster (KUBECONFIG is /dev/null, and `helm template`
// without --validate is offline) and never writes into the chart. Secret
// values files are replaced by masked copies, in a temporary directory, that
// keep their keys and set every value to Secret, so templates that need a
// secret render without one: no secret, encrypted or not, reaches helm.
func Render(ctx context.Context, c Chart) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "codegraph-deploy-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	args := []string{"template", "release", c.Dir}
	for _, v := range c.Values {
		args = append(args, "-f", inChart(c.Dir, v))
	}
	for i, s := range c.Secrets {
		data, err := os.ReadFile(inChart(c.Dir, s))
		if err != nil {
			return nil, err
		}
		m, err := maskSecrets(data)
		if err != nil {
			return nil, fmt.Errorf("deploy: %s: %w", s, err)
		}
		p := filepath.Join(tmp, fmt.Sprintf("secrets-%d.yaml", i))
		if err := os.WriteFile(p, m, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "-f", p)
	}
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("deploy: helm template %s: %v: %s", c.Dir, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

func inChart(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// maskSecrets keeps a values file's keys and replaces every value with
// Secret; sops' own metadata is dropped.
func maskSecrets(data []byte) ([]byte, error) {
	var v map[string]any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	delete(v, "sops")
	return yaml.Marshal(mask(v))
}

func mask(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = mask(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = mask(e)
		}
		return x
	default:
		return Secret
	}
}
