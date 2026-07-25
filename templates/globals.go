package templates

import (
	"context"
	"fmt"
	htmltpl "html/template"
	"reflect"
	"sync"
	texttpl "text/template"
)

// Option configures [Load] and [LoadSources].
type Option func(*options)

type options struct {
	funcs   []FuncMap
	globals GlobalsFunc
}

// GlobalsFunc builds one binding's functions and must return the same names and signatures on every call.
type GlobalsFunc func(b *Binding) FuncMap

// Funcs adds template functions. Later maps override earlier ones,
// and all of them override [DefaultFuncs].
func Funcs(funcs FuncMap) Option {
	return func(o *options) { o.funcs = append(o.funcs, funcs) }
}

// Globals registers functions that read the current render context through [Binding.Ctx].
//
//	templates.Globals(func(b *templates.Binding) templates.FuncMap {
//		return templates.FuncMap{
//			"currentUser": func() (*User, error) { return userFrom(b.Ctx()) },
//		}
//	})
//
// Sets configured with Globals render through pooled clones.
func Globals(build GlobalsFunc) Option {
	return func(o *options) { o.globals = build }
}

// Binding exposes one render's context to functions built by a [GlobalsFunc].
type Binding struct {
	tpl executor
	ctx context.Context
	err error
}

// Ctx returns the context of the render currently using this binding,
// or [context.Background] outside a render.
func (b *Binding) Ctx() context.Context {
	if b.ctx == nil {
		return context.Background()
	}
	return b.ctx
}

// discoverGlobals runs the builder for its names, holding a failing
// builder to a Load error rather than a panic at startup.
func discoverGlobals(build GlobalsFunc) (funcs FuncMap, err error) {
	defer func() {
		if p := recover(); p != nil {
			funcs, err = nil, fmt.Errorf("templates.Load: globals builder panicked: %v", p)
		}
	}()
	return build(&Binding{}), nil
}

type bindings struct {
	build GlobalsFunc
	// want fixes each global's signature at load time.
	want  map[string]reflect.Type
	clone func(FuncMap) (executor, error)
	pool  sync.Pool
}

func newBindings(build GlobalsFunc, first FuncMap, clone func(FuncMap) (executor, error)) *bindings {
	want := make(map[string]reflect.Type, len(first))
	for name, fn := range first {
		want[name] = reflect.TypeOf(fn)
	}
	b := &bindings{build: build, want: want, clone: clone}
	b.pool.New = func() any { return b.newBinding() }
	return b
}

func (b *bindings) newBinding() *Binding {
	bind := &Binding{}
	funcs, err := b.buildFor(bind)
	if err != nil {
		return &Binding{err: err}
	}
	tpl, err := b.cloneWith(funcs)
	if err != nil {
		return &Binding{err: err}
	}
	bind.tpl = tpl
	return bind
}

// buildFor converts builder contract violations into render errors.
func (b *bindings) buildFor(bind *Binding) (funcs FuncMap, err error) {
	defer func() {
		if p := recover(); p != nil {
			funcs, err = nil, fmt.Errorf("templates: globals builder panicked: %v", p)
		}
	}()
	funcs = b.build(bind)
	for name, want := range b.want {
		fn, ok := funcs[name]
		if !ok {
			return nil, fmt.Errorf("templates: globals builder dropped %q", name)
		}
		if got := reflect.TypeOf(fn); got != want {
			return nil, fmt.Errorf("templates: globals builder changed %q from %s to %s", name, want, got)
		}
	}
	// A name added after Load never reached the parse, and would
	// shadow a function the templates were parsed against.
	for name := range funcs {
		if _, ok := b.want[name]; !ok {
			return nil, fmt.Errorf("templates: globals builder added %q after load", name)
		}
	}
	return funcs, nil
}

func (b *bindings) cloneWith(funcs FuncMap) (tpl executor, err error) {
	defer func() {
		if p := recover(); p != nil {
			tpl, err = nil, fmt.Errorf("templates: globals are not valid template functions: %v", p)
		}
	}()
	return b.clone(funcs)
}

func (b *bindings) get(ctx context.Context) (*Binding, error) {
	bind := b.pool.Get().(*Binding)
	if bind.err != nil {
		return nil, bind.err
	}
	bind.ctx = ctx
	return bind, nil
}

func (b *bindings) put(bind *Binding) {
	bind.ctx = nil
	b.pool.Put(bind)
}

// htmlCloner preserves an unexecuted source because executed templates cannot be cloned.
func htmlCloner(pristine *htmltpl.Template) func(FuncMap) (executor, error) {
	return func(funcs FuncMap) (executor, error) {
		c, err := pristine.Clone()
		if err != nil {
			return nil, err
		}
		return c.Funcs(funcs), nil
	}
}

func textCloner(pristine *texttpl.Template) func(FuncMap) (executor, error) {
	return func(funcs FuncMap) (executor, error) {
		c, err := pristine.Clone()
		if err != nil {
			return nil, err
		}
		return c.Funcs(texttpl.FuncMap(funcs)), nil
	}
}
