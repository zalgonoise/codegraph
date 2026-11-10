package deploy

import (
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/link"
	"github.com/zalgonoise/codegraph/internal/model"
)

// databaseVar matches a variable naming a database or its host: a prefix, the
// family (DB, DATABASE, MYSQL, MARIADB, POSTGRES, PG), what it holds (NAME,
// DATABASE or, for POSTGRES, DB: the name; HOST) and a suffix. DB_NAME,
// DATABASE_HOST, MYSQL_DATABASE, POSTGRES_DB, PGHOST, LEGACY_DATABASE_NAME and
// DB_NAME_REPORTS all do; ATHENA_DATABASE and REDIS_DB do not.
var databaseVar = regexp.MustCompile(`^((?:[A-Z0-9]+_)*?)(DB|DATABASE|MYSQL|MARIADB|POSTGRES|PG)_?(NAME|DATABASE|DB|HOST)((?:_[A-Z0-9]+)*)$`)

// databasesOf reads the databases a container is configured with: each
// variable naming a database, with the host variable of the same prefix,
// family and suffix (DB_NAME_REPORTS goes with DB_HOST_REPORTS). A name that
// comes from a secret is not known, so neither is its database; a host that
// does leaves the database without an instance.
func databasesOf(env map[string]string) []model.Database {
	type slot struct{ name, host string }
	slots := map[string]*slot{}
	for k, v := range env {
		m := databaseVar.FindStringSubmatch(k)
		if m == nil || v == "" || v == Secret || m[3] == "DB" && m[2] != "POSTGRES" {
			continue
		}
		key := m[1] + "|" + m[2] + "|" + m[4]
		if slots[key] == nil {
			slots[key] = &slot{}
		}
		if m[3] == "HOST" {
			slots[key].host = v
		} else {
			slots[key].name = v
		}
	}
	seen := map[string]bool{}
	var out []model.Database
	for _, s := range slots {
		if s.name == "" {
			continue
		}
		db := model.Database{Instance: instanceOf(s.host), Name: s.name}
		if !seen[db.String()] {
			seen[db.String()] = true
			out = append(out, db)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// instanceOf names the database instance at host: the first label of a DNS
// name (a managed database's endpoint names the instance first), else the
// host itself (an in-cluster Service, an address).
func instanceOf(host string) string {
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || net.ParseIP(host) != nil {
		return host
	}
	label, _, _ := strings.Cut(host, ".")
	return label
}

// DatabasesOf returns the databases c is configured with: those recorded, or,
// in a deployment read before they were, those its variables name.
func (c *Container) DatabasesOf() []model.Database {
	if c.Databases != nil {
		return c.Databases
	}
	return databasesOf(c.Env)
}

// Databases returns the databases each analysed service's containers are
// configured with, by service (none without a deployment).
func (d *Deployment) Databases() map[string][]model.Database {
	if d == nil {
		return nil
	}
	out := map[string][]model.Database{}
	for _, w := range d.Workloads {
		for _, c := range w.Containers {
			if c.Service == "" {
				continue
			}
			for _, db := range c.DatabasesOf() {
				if !slices.Contains(out[c.Service], db) {
					out[c.Service] = append(out[c.Service], db)
				}
			}
		}
	}
	return out
}

// Jobs lists the programs d runs on a schedule: each main container of a
// workload with a schedule (a CronJob) that runs an analysed service.
func (d *Deployment) Jobs() []model.Job {
	if d == nil {
		return nil
	}
	var out []model.Job
	for i := range d.Workloads {
		w := &d.Workloads[i]
		if w.Schedule == "" {
			continue
		}
		for _, c := range w.Mains() {
			if c.Service != "" {
				out = append(out, model.Job{Service: c.Service, Name: w.Name, Schedule: w.Schedule,
					Command: append(append([]string(nil), c.Command...), c.Args...)})
			}
		}
	}
	return out
}

// LinkOptions is what linking learns from d: the databases each service is
// configured with and the jobs it runs on a schedule.
func (d *Deployment) LinkOptions() link.Options {
	return link.Options{Databases: d.Databases(), Jobs: d.Jobs()}
}
