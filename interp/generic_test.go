package interp

import (
	"reflect"
	"testing"
)

func TestGenericFuncDeclare(t *testing.T) {
	i := New(Options{})
	_, err := i.Eval("func Hello[T comparable](v T) *T {\n\treturn &v\n}")
	if err != nil {
		t.Error(err)
	}
	res, err := i.Eval("Hello(3)")
	if err != nil {
		t.Error(err)
	}
	if res.Elem().Interface() != 3 {
		t.Error("expected &(3), got", res)
	}
}

func TestGenericFuncBasic(t *testing.T) {
	i := New(Options{})
	err := i.Use(Exports{
		"guthib.com/generic/generic": map[string]reflect.Value{
			"Hello": reflect.ValueOf(GenericFunc("func Hello[T comparable](v T) *T {\n\treturn &v\n}")),
		},
	})
	if err != nil {
		t.Error(err)
	}
	res, err := i.Eval("generic.Hello(3)")
	if err != nil {
		t.Error(err)
	}
	if res.Elem().Interface() != 3 {
		t.Error("expected &(3), got", res)
	}
}

func TestGenericFuncNoDotImport(t *testing.T) {
	i := New(Options{})
	err := i.Use(Exports{
		"guthib.com/generic/generic": map[string]reflect.Value{
			"Hello": reflect.ValueOf(GenericFunc("func Hello[T any](v T) { println(v) }")),
		},
	})
	if err != nil {
		t.Error(err)
	}
	_, err = i.Eval(`
import "guthib.com/generic"
func main() { generic.Hello(3) }
`)
	if err != nil {
		t.Error(err)
	}
}

func TestGenericFuncDotImport(t *testing.T) {
	i := New(Options{})
	err := i.Use(Exports{
		"guthib.com/generic/generic": map[string]reflect.Value{
			"Hello": reflect.ValueOf(GenericFunc("func Hello[T any](v T) { println(v) }")),
		},
	})
	if err != nil {
		t.Error(err)
	}
	_, err = i.Eval(`
import . "guthib.com/generic"
func main() { Hello(3) }
`)
	if err != nil {
		t.Error(err)
	}
}

func TestGenericFuncComplex(t *testing.T) {
	i := New(Options{})
	done := false
	err := i.Use(Exports{
		"guthib.com/generic/generic": map[string]reflect.Value{
			"Do":    reflect.ValueOf(func() { done = true }),
			"Hello": reflect.ValueOf(GenericFunc("func Hello[T comparable, F any](v T, f func(a T) F) *T {\n\tDo(); return &v\n}")),
		},
	})
	i.ImportUsed()
	if err != nil {
		t.Error(err)
	}
	res, err := i.Eval("generic.Hello[int, bool](3, func(a int) bool { return true })")
	if err != nil {
		t.Error(err)
	}
	if res.Elem().Interface() != 3 {
		t.Error("expected &(3), got", res)
	}
	if !done {
		t.Error("!done")
	}
}

func TestGenericCallTwice(t *testing.T) {
	i := New(Options{})
	_, err := i.Eval(`
func Do[T any](v T) { println(v) }
func Hello[T any](v T) { Do(v) }
func main() { Hello[int](3) }
`)
	if err != nil {
		t.Error(err)
	}
}

func TestGenericFuncTwice(t *testing.T) {
	i := New(Options{})
	err := i.Use(Exports{
		"guthib.com/generic/generic": map[string]reflect.Value{
			"Do":    reflect.ValueOf(GenericFunc("func Do[T any](v T) { println(v) }")),
			"Hello": reflect.ValueOf(GenericFunc("func Hello[T any](v T) { Do(v) }")),
		},
	})
	i.ImportUsed()
	if err != nil {
		t.Error(err)
	}
	_, err = i.Eval(`
func main() { generic.Hello[int](3) }
`)
	if err != nil {
		t.Error(err)
	}
}

func TestGenericFuncInfer(t *testing.T) {
	i := New(Options{})
	err := i.Use(Exports{
		"guthib.com/generic/generic": map[string]reflect.Value{
			"New":   reflect.ValueOf(GenericFunc("func New[T any]() *T { return new(T) }")),
			"AddAt": reflect.ValueOf(GenericFunc("func AddAt[T any](init func(n *T)) { v := New(T); init(v); println(*v) }")),
			// "AddAt": reflect.ValueOf(GenericFunc("func AddAt[T any]() *T { v := generic.New[T](T); return v }")),
		},
	})
	i.ImportUsed()
	if err != nil {
		t.Error(err)
	}
	i.Eval(`
func main() {
// v := generic.AddAt()
// v := generic.AddAt[int]() // crashes!
generic.AddAt(func(w *int) { *w = 3 }) // for init version
// v := generic.New[int]() // direct calling works
}
`)
}

func TestGenericFuncInferDirect(t *testing.T) {
	i := New(Options{})
	// note: this version behaves identically to non-direct Export version
	i.Eval(`
func New[T any]() *T { return new(T) }
func AddAt[T any](init func(n *T)) { v := New(T); init(v); println(*v) }
// func AddAt[T any]() *T { v := New(T); return v }
func main() {
// v := AddAt[int]() // crashes!
// v := AddAt() // not enough info for non-init version
AddAt(func(w *int) { *w = 3 }) // for init version
// v := New[int]() // direct calling works
}
`)
	// if err != nil {
	// 	t.Error(err)
	// }
}

func TestCallPackageFunc(t *testing.T) {
	i := New(Options{})
	done := false
	err := i.Use(Exports{
		"guthib.com/generic/generic": map[string]reflect.Value{
			"Do": reflect.ValueOf(func() { done = true }),
		},
	})
	if err != nil {
		t.Error(err)
	}
	_, err = i.Eval("import . \"guthib.com/generic\"\nfunc Hello() { Do() }\nfunc main() { Hello() }")
	if err != nil {
		t.Error(err)
	}
	if !done {
		t.Error("!done")
	}
}
