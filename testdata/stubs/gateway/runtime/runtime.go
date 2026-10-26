// Package runtime is a minimal offline stand-in for grpc-gateway's runtime:
// the ServeMux the generated code registers routes on, and the patterns it
// builds from op codes. ServeHTTP dispatches to every handler, which keeps them
// reachable.
package runtime

import "net/http"

type HandlerFunc func(w http.ResponseWriter, r *http.Request, pathParams map[string]string)

type Pattern struct {
	ops  []int
	pool []string
	verb string
}

func NewPattern(version int, ops []int, pool []string, verb string) (Pattern, error) {
	return Pattern{ops: ops, pool: pool, verb: verb}, nil
}

func MustPattern(p Pattern, err error) Pattern {
	if err != nil {
		panic(err)
	}
	return p
}

type ServeMux struct {
	handlers []HandlerFunc
}

func NewServeMux() *ServeMux { return &ServeMux{} }

func (s *ServeMux) Handle(meth string, pat Pattern, h HandlerFunc) {
	s.handlers = append(s.handlers, h)
}

func (s *ServeMux) HandlePath(meth string, pathPattern string, h HandlerFunc) error {
	s.handlers = append(s.handlers, h)
	return nil
}

func (s *ServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, h := range s.handlers {
		h(w, r, nil)
	}
}
