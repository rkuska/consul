package config

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	// Basically just testing that injection of the extra
	// source works.
	devMode := true
	builderOpts := LoadOpts{
		// putting this in dev mode so that the config validates
		// without having to specify a data directory
		DevMode: &devMode,
		DefaultConfig: FileSource{
			Name:   "test",
			Format: "hcl",
			Data:   `node_name = "hobbiton"`,
		},
		Overrides: []Source{
			FileSource{
				Name:   "overrides",
				Format: "json",
				Data:   `{"check_reap_interval": "1ms"}`,
			},
		},
	}

	result, err := Load(builderOpts)
	require.NoError(t, err)
	require.Empty(t, result.Warnings)
	cfg := result.RuntimeConfig
	require.NotNil(t, cfg)
	require.Equal(t, "hobbiton", cfg.NodeName)
	require.Equal(t, 1*time.Millisecond, cfg.CheckReapInterval)
}

func TestShouldParseFile(t *testing.T) {
	var testcases = []struct {
		filename     string
		configFormat string
		expected     bool
	}{
		{filename: "config.json", expected: true},
		{filename: "config.hcl", expected: true},
		{filename: "config", configFormat: "hcl", expected: true},
		{filename: "config.js", configFormat: "json", expected: true},
		{filename: "config.yaml", expected: false},
	}

	for _, tc := range testcases {
		name := fmt.Sprintf("filename=%s, format=%s", tc.filename, tc.configFormat)
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.expected, shouldParseFile(tc.filename, tc.configFormat))
		})
	}
}

func TestNewBuilder_PopulatesSourcesFromConfigFiles(t *testing.T) {
	paths := setupConfigFiles(t)

	b, err := NewBuilder(LoadOpts{ConfigFiles: paths})
	require.NoError(t, err)

	expected := []Source{
		FileSource{Name: paths[0], Format: "hcl", Data: "content a"},
		FileSource{Name: paths[1], Format: "json", Data: "content b"},
		FileSource{Name: filepath.Join(paths[3], "a.hcl"), Format: "hcl", Data: "content a"},
		FileSource{Name: filepath.Join(paths[3], "b.json"), Format: "json", Data: "content b"},
	}
	require.Equal(t, expected, b.Sources)
	require.Len(t, b.Warnings, 2)
}

func TestNewBuilder_PopulatesSourcesFromConfigFiles_WithConfigFormat(t *testing.T) {
	paths := setupConfigFiles(t)

	b, err := NewBuilder(LoadOpts{ConfigFiles: paths, ConfigFormat: "hcl"})
	require.NoError(t, err)

	expected := []Source{
		FileSource{Name: paths[0], Format: "hcl", Data: "content a"},
		FileSource{Name: paths[1], Format: "hcl", Data: "content b"},
		FileSource{Name: paths[2], Format: "hcl", Data: "content c"},
		FileSource{Name: filepath.Join(paths[3], "a.hcl"), Format: "hcl", Data: "content a"},
		FileSource{Name: filepath.Join(paths[3], "b.json"), Format: "hcl", Data: "content b"},
		FileSource{Name: filepath.Join(paths[3], "c.yaml"), Format: "hcl", Data: "content c"},
	}
	require.Equal(t, expected, b.Sources)
}

// TODO: this would be much nicer with gotest.tools/fs
func setupConfigFiles(t *testing.T) []string {
	t.Helper()
	path, err := ioutil.TempDir("", t.Name())
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(path) })

	subpath := filepath.Join(path, "sub")
	err = os.Mkdir(subpath, 0755)
	require.NoError(t, err)

	for _, dir := range []string{path, subpath} {
		err = ioutil.WriteFile(filepath.Join(dir, "a.hcl"), []byte("content a"), 0644)
		require.NoError(t, err)

		err = ioutil.WriteFile(filepath.Join(dir, "b.json"), []byte("content b"), 0644)
		require.NoError(t, err)

		err = ioutil.WriteFile(filepath.Join(dir, "c.yaml"), []byte("content c"), 0644)
		require.NoError(t, err)
	}
	return []string{
		filepath.Join(path, "a.hcl"),
		filepath.Join(path, "b.json"),
		filepath.Join(path, "c.yaml"),
		subpath,
	}
}

func TestBuilder_BuildAndValidate_NodeName(t *testing.T) {
	type testCase struct {
		name         string
		nodeName     string
		expectedWarn string
	}

	fn := func(t *testing.T, tc testCase) {
		b, err := NewBuilder(LoadOpts{
			FlagValues: Config{
				NodeName: pString(tc.nodeName),
				DataDir:  pString("dir"),
			},
		})
		patchBuilderShims(b)
		require.NoError(t, err)
		_, err = b.BuildAndValidate()
		require.NoError(t, err)
		require.Len(t, b.Warnings, 1)
		require.Contains(t, b.Warnings[0], tc.expectedWarn)
	}

	var testCases = []testCase{
		{
			name:         "invalid character - unicode",
			nodeName:     "🐼",
			expectedWarn: `Node name "🐼" will not be discoverable via DNS due to invalid characters`,
		},
		{
			name:         "invalid character - slash",
			nodeName:     "thing/other/ok",
			expectedWarn: `Node name "thing/other/ok" will not be discoverable via DNS due to invalid characters`,
		},
		{
			name:         "too long",
			nodeName:     strings.Repeat("a", 66),
			expectedWarn: "due to it being too long.",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fn(t, tc)
		})
	}
}

func patchBuilderShims(b *Builder) {
	b.opts.hostname = func() (string, error) {
		return "thehostname", nil
	}
	b.opts.getPrivateIPv4 = func() ([]*net.IPAddr, error) {
		return []*net.IPAddr{ipAddr("10.0.0.1")}, nil
	}
	b.opts.getPublicIPv6 = func() ([]*net.IPAddr, error) {
		return []*net.IPAddr{ipAddr("dead:beef::1")}, nil
	}
}

func TestLoad_HTTPMaxConnsPerClientExceedsRLimit(t *testing.T) {
	hcl := `
		limits{
			# We put a very high value to be sure to fail
			# This value is more than max on Windows as well
			http_max_conns_per_client = 16777217
		}`

	opts := LoadOpts{
		DefaultConfig: FileSource{
			Name:   "test",
			Format: "hcl",
			Data: `
		    ae_interval = "1m"
		    data_dir="/tmp/00000000001979"
			bind_addr = "127.0.0.1"
			advertise_addr = "127.0.0.1"
			datacenter = "dc1"
			bootstrap = true
			server = true
			node_id = "00000000001979"
			node_name = "Node-00000000001979"
		`,
		},
		HCL: []string{hcl},
	}

	_, err := Load(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "but limits.http_max_conns_per_client: 16777217 needs at least 16777237")
}
