// Package bundle packs a run into one compressed file that can be shared, and
// reads a run back from a directory or from such a file.
//
// A bundle is a tar archive, compressed with gzip (the default) or zstd, laid
// out like a run directory, so that extracting it gives a run:
//
//	bundle.json            the metadata: format, versions, date, options
//	<service>/manifest.json
//	deploy/deploy.json     what an environment runs, when the run has it
//	inventory.json         the organisation's inventory, when given
//	_system/system.html    the explorer, drawn from the bundle's own contents
//
// The system view (system.json) is left out: every reader links the manifests
// again on load, in about a second. The options leave out what should not
// travel: the values an environment configures (NoSettings), and every
// reference to a file and line (NoPositions).
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/inventory"
	"github.com/zalgonoise/codegraph/internal/link"
	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/render"
)

// Format is the bundle layout this package writes and the newest it reads.
const Format = 1

// Compressions.
const (
	Gzip = "gzip"
	Zstd = "zstd"
)

// Names of the files in a bundle (and in a run directory).
const (
	MetadataFile  = "bundle.json"
	DeployFile    = "deploy/deploy.json"
	InventoryFile = "inventory.json"
	ExplorerFile  = "_system/system.html"
	manifestFile  = "manifest.json"
)

// maxEntry bounds what one file of a bundle may decompress to.
const maxEntry = 1 << 30

// Metadata describes a bundle: bundle.json.
type Metadata struct {
	Format      int       `json:"format"`
	Codegraph   string    `json:"codegraph"` // the version that wrote the bundle
	Created     time.Time `json:"created"`
	Compression string    `json:"compression"`
	Services    int       `json:"services"`
	Libraries   int       `json:"libraries,omitempty"`
	AnalysedBy  []string  `json:"analysedBy,omitempty"`  // the codegraph versions that wrote its manifests
	Environment string    `json:"environment,omitempty"` // the deployment's, when it has one
	Inventory   bool      `json:"inventory,omitempty"`
	NoSettings  bool      `json:"noSettings,omitempty"`  // configured values left out
	NoPositions bool      `json:"noPositions,omitempty"` // file and line references left out
}

// Run is what a run holds, read from a directory or a bundle.
type Run struct {
	Manifests []*model.Manifest
	Deploy    *deploy.Deployment   // nil without a deployment
	Inventory *inventory.Inventory // nil without an inventory
	Explorer  []byte               // a bundle's explorer page; nil for a directory
	Meta      *Metadata            // a bundle's metadata; nil for a directory
}

// Options shape a bundle.
type Options struct {
	Compression string    // Gzip (default) or Zstd
	NoSettings  bool      // leave out the values an environment configures
	NoPositions bool      // leave out file and line references
	Version     string    // the codegraph version writing it
	Now         time.Time // the creation date (time.Now when zero)
}

// IsBundle reports whether path is a file (a bundle) rather than a run
// directory.
func IsBundle(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Load reads a run from a bundle or a run directory. inventoryFile, when
// set, is read instead of the run's own inventory.
func Load(path, inventoryFile string) (*Run, error) {
	var r *Run
	var err error
	if IsBundle(path) {
		r, err = Read(path)
	} else {
		r, err = ReadDir(path)
	}
	if err != nil {
		return nil, err
	}
	if inventoryFile != "" {
		if r.Inventory, err = ReadInventory(inventoryFile); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// ReadDir reads a run directory: its manifests (<dir>/<service>/manifest.json),
// its deployment (deploy/deploy.json) and an inventory: <dir>/inventory.json,
// else the one the Makefile writes next to the run (<dir>/../inventory).
func ReadDir(dir string) (*Run, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", manifestFile))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no manifests in %s: run `make graph` first, or point -run at a run directory or a bundle", dir)
	}
	sort.Strings(paths)
	r := &Run{}
	seen := map[string]string{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if err := r.addManifest(data, p, seen); err != nil {
			return nil, err
		}
	}
	if file := filepath.Join(dir, filepath.FromSlash(DeployFile)); fileExists(file) {
		if r.Deploy, err = deploy.ReadFile(file); err != nil {
			return nil, err
		}
	}
	for _, file := range []string{filepath.Join(dir, InventoryFile), filepath.Join(filepath.Dir(filepath.Clean(dir)), "inventory", InventoryFile)} {
		if fileExists(file) {
			if r.Inventory, err = ReadInventory(file); err != nil {
				return nil, err
			}
			break
		}
	}
	return r, nil
}

// ReadInventory reads an inventory.json, or the one in a directory.
func ReadInventory(file string) (*inventory.Inventory, error) {
	if st, err := os.Stat(file); err == nil && st.IsDir() {
		file = filepath.Join(file, InventoryFile)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &inv, nil
}

func (r *Run) addManifest(data []byte, name string, seen map[string]string) error {
	var m model.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if prev, ok := seen[m.Service]; ok {
		return fmt.Errorf("service %q defined twice (%s and %s)", m.Service, prev, name)
	}
	seen[m.Service] = name
	r.Manifests = append(r.Manifests, &m)
	return nil
}

// Write packs r into w, applying opts, and returns the bundle's metadata.
func Write(w io.Writer, r *Run, opts Options) (*Metadata, error) {
	if opts.Compression == "" {
		opts.Compression = Gzip
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	ms, dep, inv, err := r.stripped(opts)
	if err != nil {
		return nil, err
	}
	meta := &Metadata{
		Format: Format, Codegraph: opts.Version, Created: opts.Now.UTC().Truncate(time.Second), Compression: opts.Compression,
		Inventory: inv != nil, NoSettings: opts.NoSettings, NoPositions: opts.NoPositions,
	}
	versions := map[string]bool{}
	for _, m := range ms {
		if m.Library {
			meta.Libraries++
		} else {
			meta.Services++
		}
		if m.Codegraph != "" && !versions[m.Codegraph] {
			versions[m.Codegraph] = true
			meta.AnalysedBy = append(meta.AnalysedBy, m.Codegraph)
		}
	}
	sort.Strings(meta.AnalysedBy)
	if dep != nil {
		meta.Environment = dep.Environment
	}

	files := map[string][]byte{}
	add := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		files[name] = append(data, '\n')
		return nil
	}
	if err := add(MetadataFile, meta); err != nil {
		return nil, err
	}
	for _, m := range ms {
		if strings.ContainsAny(m.Service, `/\`) || m.Service == "" || m.Service == "." || m.Service == ".." {
			return nil, fmt.Errorf("service name %q cannot be a directory", m.Service)
		}
		if err := add(m.Service+"/"+manifestFile, m); err != nil {
			return nil, err
		}
	}
	if dep != nil {
		if err := add(DeployFile, dep); err != nil {
			return nil, err
		}
	}
	if inv != nil {
		if err := add(InventoryFile, inv); err != nil {
			return nil, err
		}
	}
	page, err := render.Explorer(link.Link(ms), ms, dep)
	if err != nil {
		return nil, err
	}
	files[ExplorerFile] = []byte(page)

	var zw io.WriteCloser
	switch opts.Compression {
	case Gzip:
		zw, err = gzip.NewWriterLevel(w, gzip.BestCompression)
	case Zstd:
		zw, err = zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	default:
		return nil, fmt.Errorf("unknown compression %q (want %s or %s)", opts.Compression, Gzip, Zstd)
	}
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { // the metadata first, so a reader meets it before anything else
		if (names[i] == MetadataFile) != (names[j] == MetadataFile) {
			return names[i] == MetadataFile
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), ModTime: meta.Created, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return meta, nil
}

// Read reads a bundle: gzip or zstd, told by its first bytes.
func Read(path string) (*Run, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := ReadFrom(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// ReadFrom reads a bundle from a stream.
func ReadFrom(in io.Reader) (*Run, error) {
	br := &peekReader{r: in}
	magic, err := br.peek(4)
	if err != nil {
		return nil, errors.New("not a bundle: too short")
	}
	var zr io.Reader
	switch {
	case magic[0] == 0x1f && magic[1] == 0x8b:
		g, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		defer g.Close()
		zr = g
	case bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		z, err := zstd.NewReader(br)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		zr = z
	default:
		return nil, errors.New("not a bundle: neither gzip nor zstd (codegraph bundle writes one)")
	}
	r := &Run{}
	seen := map[string]string{}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > maxEntry {
			return nil, fmt.Errorf("%s: %d bytes, more than a bundle file may hold", hdr.Name, hdr.Size)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxEntry))
		if err != nil {
			return nil, err
		}
		name := path.Clean(hdr.Name)
		switch {
		case name == MetadataFile:
			var meta Metadata
			if err := json.Unmarshal(data, &meta); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if meta.Format > Format || meta.Format < 1 {
				return nil, fmt.Errorf("bundle format %d, this codegraph reads format %d: use a codegraph as new as the one that wrote it (%s)",
					meta.Format, Format, meta.Codegraph)
			}
			r.Meta = &meta
		case name == DeployFile:
			var d deploy.Deployment
			if err := json.Unmarshal(data, &d); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			r.Deploy = &d
		case name == InventoryFile:
			var inv inventory.Inventory
			if err := json.Unmarshal(data, &inv); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			r.Inventory = &inv
		case name == ExplorerFile:
			r.Explorer = data
		case path.Base(name) == manifestFile && strings.Count(name, "/") == 1:
			if err := r.addManifest(data, name, seen); err != nil {
				return nil, err
			}
		}
	}
	if r.Meta == nil {
		return nil, fmt.Errorf("not a bundle: no %s", MetadataFile)
	}
	if len(r.Manifests) == 0 {
		return nil, errors.New("the bundle holds no manifest")
	}
	sort.Slice(r.Manifests, func(i, j int) bool { return r.Manifests[i].Service < r.Manifests[j].Service })
	return r, nil
}

// ExplorerPage writes a bundle's explorer page to a cache directory (once per
// bundle content) and returns its path, for answers to link to.
func ExplorerPage(r *Run, bundlePath string) (string, error) {
	if len(r.Explorer) == 0 {
		return "", errors.New("the bundle has no explorer page")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(r.Explorer)
	name := filepath.Base(bundlePath)
	for _, ext := range []string{".gz", ".zst", ".zstd", ".tgz", ".tzst", ".tar"} {
		name = strings.TrimSuffix(name, ext)
	}
	dir := filepath.Join(cache, "codegraph", "bundles", name+"-"+hex.EncodeToString(sum[:6]))
	file := filepath.Join(dir, "system.html")
	if fileExists(file) {
		return file, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, r.Explorer, 0o644); err != nil {
		return "", err
	}
	return file, os.Rename(tmp, file)
}

// stripped returns copies of the run's contents with opts' omissions applied.
func (r *Run) stripped(opts Options) ([]*model.Manifest, *deploy.Deployment, *inventory.Inventory, error) {
	ms := make([]*model.Manifest, len(r.Manifests))
	for i, m := range r.Manifests {
		var c model.Manifest
		if err := clone(m, &c, opts.NoPositions); err != nil {
			return nil, nil, nil, err
		}
		ms[i] = &c
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Service < ms[j].Service })
	var dep *deploy.Deployment
	if r.Deploy != nil {
		dep = &deploy.Deployment{}
		if err := clone(r.Deploy, dep, false); err != nil {
			return nil, nil, nil, err
		}
		if opts.NoPositions {
			dep.Source = "" // the chart's directory on the machine that rendered it
		}
		if opts.NoSettings {
			hideSettings(dep)
		}
	}
	var inv *inventory.Inventory
	if r.Inventory != nil {
		inv = &inventory.Inventory{}
		if err := clone(r.Inventory, inv, opts.NoPositions); err != nil {
			return nil, nil, nil, err
		}
	}
	return ms, dep, inv, nil
}

// positionKeys are the JSON fields that hold a file reference: "file:line"
// positions in a repository, and the files a declaration or an image came
// from.
var positionKeys = map[string]bool{"position": true, "positions": true, "file": true, "dockerfile": true}

// clone deep-copies src into dst through JSON, dropping the position fields
// when asked.
func clone(src, dst any, noPositions bool) error {
	data, err := json.Marshal(src)
	if err != nil {
		return err
	}
	if noPositions {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		if data, err = json.Marshal(dropKeys(v)); err != nil {
			return err
		}
	}
	return json.Unmarshal(data, dst)
}

func dropKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if positionKeys[k] {
				delete(x, k)
				continue
			}
			x[k] = dropKeys(val)
		}
	case []any:
		for i := range x {
			x[i] = dropKeys(x[i])
		}
	}
	return v
}

// hideSettings leaves out the values an environment configures: every
// container's variables keep their names, the address check's settings and
// the mocks keep their names only.
func hideSettings(d *deploy.Deployment) {
	for i := range d.Workloads {
		for j := range d.Workloads[i].Containers {
			c := &d.Workloads[i].Containers[j]
			for k := range c.Env {
				c.Env[k] = ""
			}
		}
	}
	n := d.Network
	if n == nil {
		return
	}
	for _, pairs := range [][]deploy.Pair{n.Confirmed, n.CodeOnly, n.ConfigOnly} {
		for i := range pairs {
			for j, s := range pairs[i].Settings {
				pairs[i].Settings[j] = settingName(s)
			}
		}
	}
	for i := range n.Settled {
		n.Settled[i].Setting = settingName(n.Settled[i].Setting)
	}
	for i := range n.Mocked {
		n.Mocked[i].Setting = settingName(n.Mocked[i].Setting)
	}
}

// settingName drops the value of a "NAME=value" setting.
func settingName(s string) string {
	name, _, _ := strings.Cut(s, "=")
	return name
}

// Compression returns the compression a file name asks for: zstd for .zst
// endings, gzip otherwise.
func Compression(name string) string {
	if strings.HasSuffix(name, ".zst") || strings.HasSuffix(name, ".tzst") || strings.HasSuffix(name, ".zstd") {
		return Zstd
	}
	return Gzip
}

// peekReader lets ReadFrom look at the first bytes of a stream.
type peekReader struct {
	r   io.Reader
	buf []byte
}

func (p *peekReader) peek(n int) ([]byte, error) {
	for len(p.buf) < n {
		b := make([]byte, n-len(p.buf))
		k, err := p.r.Read(b)
		p.buf = append(p.buf, b[:k]...)
		if err != nil {
			if len(p.buf) >= n {
				break
			}
			return nil, err
		}
	}
	return p.buf[:n], nil
}

func (p *peekReader) Read(b []byte) (int, error) {
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}
	return p.r.Read(b)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
