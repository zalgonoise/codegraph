package types

type Topic string

func (t Topic) String() string { return string(t) }

type Data interface {
	Topic() Topic
	Validate() error
}
