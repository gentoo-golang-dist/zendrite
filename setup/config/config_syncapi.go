package config

import "github.com/sirupsen/logrus"

type SyncAPI struct {
	Matrix *Global `yaml:"-"`

	Database DatabaseOptions `yaml:"database,omitempty"`

	// Deprecated: ignored. Client addresses now come from X-Forwarded-For or
	// X-Real-IP, and only from peers listed in global.trusted_proxies.
	RealIPHeader string `yaml:"real_ip_header"`

	Fulltext Fulltext `yaml:"search"`
}

func (c *SyncAPI) Defaults(opts DefaultOpts) {
	c.Fulltext.Defaults(opts)
	if opts.Generate {
		if !opts.SingleDatabase {
			c.Database.ConnectionString = "file:syncapi.db"
		}
	}
}

func (c *SyncAPI) Verify(configErrs *ConfigErrors) {
	if c.RealIPHeader != "" {
		logrus.Warn("sync_api.real_ip_header is deprecated and ignored: X-Forwarded-For and X-Real-IP are honored from the proxies in global.trusted_proxies")
	}
	c.Fulltext.Verify(configErrs)
	if c.Matrix.DatabaseOptions.ConnectionString == "" {
		checkNotEmpty(configErrs, "sync_api.database", string(c.Database.ConnectionString))
	}
}

type Fulltext struct {
	// RebuildOnStartup creates a fresh local index from PostgreSQL before serving.
	RebuildOnStartup bool   `yaml:"rebuild_on_startup"`
	Enabled          bool   `yaml:"enabled"`
	IndexPath        Path   `yaml:"index_path"`
	InMemory         bool   `yaml:"in_memory"` // only useful in tests
	Language         string `yaml:"language"`  // the language to use when analyzing content
}

func (f *Fulltext) Defaults(opts DefaultOpts) {
	f.Enabled = false
	f.IndexPath = "./searchindex"
	f.Language = "en"
}

func (f *Fulltext) Verify(configErrs *ConfigErrors) {
	if !f.Enabled {
		return
	}
	checkNotEmpty(configErrs, "syncapi.search.index_path", string(f.IndexPath))
	checkNotEmpty(configErrs, "syncapi.search.language", f.Language)
}
