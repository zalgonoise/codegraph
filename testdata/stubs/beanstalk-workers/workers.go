// Package workers is a minimal offline stand-in for
// github.com/mikegleasonjr/workers: a mux running one handler per beanstalkd
// tube.
package workers

type Job struct {
	ID   uint64
	Tube string
	Body []byte
}

type Handler interface {
	Work(*Job)
}

type HandlerFunc func(*Job)

func (f HandlerFunc) Work(j *Job) { f(j) }

type WorkMux struct{ handlers map[string]Handler }

func NewWorkMux() *WorkMux { return &WorkMux{handlers: map[string]Handler{}} }

func (mux *WorkMux) Handle(tube string, handler Handler) { mux.handlers[tube] = handler }

func (mux *WorkMux) Work(j *Job) {
	if h, ok := mux.handlers[j.Tube]; ok {
		h.Work(j)
	}
}

func ConnectAndWork(network, addr string, handler Handler) error { return nil }
