// Package beanstalk is a minimal offline stand-in for github.com/kr/beanstalk:
// a connection, the tubes named on it, and the tube sets reserved from.
package beanstalk

import "time"

type Conn struct {
	Tube
	TubeSet
}

func Dial(network, addr string) (*Conn, error) {
	c := &Conn{}
	c.Tube = Tube{Conn: c, Name: "default"}
	c.TubeSet = *NewTubeSet(c, "default")
	return c, nil
}

func (c *Conn) Delete(id uint64) error { return nil }

// Tube is a name on a connection, which jobs are put on.
type Tube struct {
	Conn *Conn
	Name string
}

func (t *Tube) Put(body []byte, pri uint32, delay, ttr time.Duration) (id uint64, err error) {
	return 1, nil
}

// TubeSet is the names a connection reserves jobs from.
type TubeSet struct {
	Conn *Conn
	Name map[string]bool
}

func NewTubeSet(c *Conn, name ...string) *TubeSet {
	ts := &TubeSet{Conn: c, Name: map[string]bool{}}
	for _, n := range name {
		ts.Name[n] = true
	}
	return ts
}

func (t *TubeSet) Reserve(timeout time.Duration) (id uint64, body []byte, err error) {
	return 0, nil, nil
}
