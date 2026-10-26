package transport

import (
	"go/constant"
	"go/token"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/resolve"
)

// grpc-gateway routes. The generated code registers each route on a
// runtime.ServeMux with a package-level pattern built from op codes:
//
//	var pattern_Authz_Login_0 = runtime.MustPattern(runtime.NewPattern(1, []int{2, 0, 2, 1}, []string{"v1", "login"}, ""))
//	mux.Handle("POST", pattern_Authz_Login_0, func(w http.ResponseWriter, req *http.Request, pathParams map[string]string) { … })
//
// gatewayPaths replays the op codes into the route's path template.

// Op codes of grpc-gateway's utilities.OpCode.
const (
	gwOpNop     = 0
	gwOpPush    = 1 // any one segment
	gwOpLitPush = 2 // the literal pool[operand]
	gwOpPushM   = 3 // any number of segments
	gwOpConcatN = 4 // join the top operand segments
	gwOpCapture = 5 // bind the top segment to the variable pool[operand]
)

// gatewayPaths returns the path templates a runtime.Pattern value stands for
// ("/v1/login", "/v1/{name=projects/*}:cancel"), or "*" when it cannot be told.
func (d *Detector) gatewayPaths(v ssa.Value) []string {
	var out []string
	for _, call := range newPatternCalls(d.Resolver, v) {
		if p, ok := gatewayPath(call); ok {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{model.Unknown}
	}
	return out
}

// newPatternCalls finds the runtime.NewPattern calls a pattern value comes
// from: through the package variable it is stored in, and MustPattern.
func newPatternCalls(res *resolve.Resolver, v ssa.Value) []*ssa.CallCommon {
	var out []*ssa.CallCommon
	var visit func(v ssa.Value, depth int)
	visit = func(v ssa.Value, depth int) {
		if depth > 4 {
			return
		}
		switch x := v.(type) {
		case *ssa.UnOp:
			if x.Op == token.MUL {
				for _, val := range res.Stores(x.X) {
					visit(val, depth+1)
				}
			}
		case *ssa.Extract:
			visit(x.Tuple, depth+1)
		case *ssa.Call:
			callee := x.Call.StaticCallee()
			if callee == nil {
				return
			}
			switch callee.Name() {
			case "NewPattern":
				out = append(out, &x.Call)
			case "MustPattern":
				if len(x.Call.Args) > 0 {
					visit(x.Call.Args[0], depth+1)
				}
			}
		}
	}
	visit(v, 0)
	return out
}

// gatewayPath replays NewPattern(version, ops, pool, verb).
func gatewayPath(c *ssa.CallCommon) (string, bool) {
	if len(c.Args) < 4 {
		return "", false
	}
	opVals, ok1 := resolve.SliceLiteral(c.Args[1])
	poolVals, ok2 := resolve.SliceLiteral(c.Args[2])
	if !ok1 || !ok2 {
		return "", false
	}
	ops := make([]int64, len(opVals))
	for i, v := range opVals {
		n, ok := constInt(v)
		if !ok {
			return "", false
		}
		ops[i] = n
	}
	pool := make([]string, len(poolVals))
	for i, v := range poolVals {
		s, ok := constString(v)
		if !ok {
			return "", false
		}
		pool[i] = s
	}
	verb, _ := constString(c.Args[3])
	return decodePattern(ops, pool, verb)
}

// decodePattern rebuilds a path template from a pattern's op codes, pairs of
// (op, operand), and its pool of literals and variable names.
func decodePattern(ops []int64, pool []string, verb string) (string, bool) {
	if len(ops)%2 != 0 {
		return "", false
	}
	var stack []string
	for i := 0; i < len(ops); i += 2 {
		op, operand := ops[i], ops[i+1]
		switch op {
		case gwOpNop:
		case gwOpPush:
			stack = append(stack, "*")
		case gwOpPushM:
			stack = append(stack, "**")
		case gwOpLitPush:
			if operand < 0 || int(operand) >= len(pool) {
				return "", false
			}
			stack = append(stack, pool[operand])
		case gwOpConcatN:
			if operand < 0 || int(operand) > len(stack) {
				return "", false
			}
			n := len(stack) - int(operand)
			stack = append(stack[:n], strings.Join(stack[n:], "/"))
		case gwOpCapture:
			if len(stack) == 0 || operand < 0 || int(operand) >= len(pool) {
				return "", false
			}
			seg := stack[len(stack)-1]
			v := "{" + pool[operand]
			if seg != "*" {
				v += "=" + seg
			}
			stack[len(stack)-1] = v + "}"
		default:
			return "", false
		}
	}
	path := "/" + strings.Join(stack, "/")
	if verb != "" {
		path += ":" + verb
	}
	return path, true
}

func constString(v ssa.Value) (string, bool) {
	c, ok := v.(*ssa.Const)
	if !ok || c.Value == nil || c.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(c.Value), true
}

func constInt(v ssa.Value) (int64, bool) {
	c, ok := v.(*ssa.Const)
	if !ok || c.Value == nil || c.Value.Kind() != constant.Int {
		return 0, false
	}
	return constant.Int64Val(c.Value)
}
