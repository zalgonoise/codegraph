package transport

import "testing"

// Patterns as protoc-gen-grpc-gateway generates them for these http rules.
func TestDecodePattern(t *testing.T) {
	for _, tc := range []struct {
		ops  []int64
		pool []string
		verb string
		want string
	}{
		{[]int64{2, 0, 2, 1}, []string{"v1", "login"}, "", "/v1/login"},                                                                       // post: "/v1/login"
		{[]int64{2, 0, 2, 1, 1, 0, 4, 1, 5, 2, 2, 3}, []string{"v1", "accounts", "account", "credit"}, "", "/v1/accounts/{account}/credit"},   // a variable
		{[]int64{2, 0, 2, 1, 1, 0, 2, 2, 1, 0, 4, 4, 5, 3}, []string{"v1", "projects", "books", "name"}, "", "/v1/{name=projects/*/books/*}"}, // a variable over a sub-pattern
		{[]int64{2, 0, 3, 0, 4, 1, 5, 1}, []string{"files", "path"}, "", "/files/{path=**}"},                                                  // a multi-segment variable
		{[]int64{2, 0, 1, 0, 4, 1, 5, 1}, []string{"v1", "operation"}, "cancel", "/v1/{operation}:cancel"},                                    // a custom verb
	} {
		got, ok := decodePattern(tc.ops, tc.pool, tc.verb)
		if !ok || got != tc.want {
			t.Errorf("decodePattern(%v, %v, %q) = %q, %v; want %q", tc.ops, tc.pool, tc.verb, got, ok, tc.want)
		}
	}
	if _, ok := decodePattern([]int64{2, 9}, []string{"v1"}, ""); ok {
		t.Error("an operand outside the pool should fail")
	}
}
