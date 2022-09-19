package pipe

import (
	"math"
	"sync"

	"github.com/koss-null/lambda/internal/bitmap"
	"go.uber.org/atomic"
)

const (
	defaultParallelWrks = 4
	maxParallelWrks     = 256
)

// Pipe implements the pipe on any slice.
// Pipe should be initialized with New() or NewFn()
type Pipe[T any] struct {
	fn       func() func(int) (*T, bool)
	len      *int64
	valLim   *int64
	skip     func(i int)
	parallel int
}

// Slice creates a Pipe from a slice
func Slice[T any](dt []T) *Pipe[T] {
	dtCp := make([]T, len(dt))
	copy(dtCp, dt)
	bm := bitmap.NewNaive(len(dtCp))
	length := int64(len(dt))
	zero := int64(0)
	return &Pipe[T]{
		fn: func() func(int) (*T, bool) {
			return func(i int) (*T, bool) {
				if i >= len(dtCp) {
					return nil, true
				}
				return &dtCp[i], false
			}
		},
		len:      &length,
		valLim:   &zero,
		skip:     bm.SetTrue,
		parallel: defaultParallelWrks,
	}
}

// Func creates a lazy sequence d[i] = fn(i)
// fn is a function, that returns an object(T) and does it exist(bool)
// Initiating the pipe from a func you have to set either the
// output value amount using Get(n int) or
// the amount of generated values Gen(n int)
func Func[T any](fn func(i int) (T, bool)) *Pipe[T] {
	// FIXME: do we need fncache here?
	// fnCache := fnintcache.New[T]()
	bm := bitmap.NewNaive(1024)
	length := int64(-1)
	zero := int64(0)
	return &Pipe[T]{
		fn: func() func(int) (*T, bool) {
			return func(i int) (*T, bool) {
				// if res, found := fnCache.Get(i); found {
				// return res, false
				// }
				obj, exist := fn(i)
				// fnCache.Set(i, obj)
				return &obj, !exist
			}
		},
		len:      &length,
		valLim:   &zero,
		skip:     bm.SetTrue,
		parallel: defaultParallelWrks,
	}
}

// Map applies fn to each element of the underlying slice
func (p *Pipe[T]) Map(fn func(T) T) *Pipe[T] {
	return &Pipe[T]{
		fn: func() func(i int) (*T, bool) {
			return func(i int) (*T, bool) {
				if obj, skipped := p.fn()(i); !skipped {
					*obj = fn(*obj)
					return obj, false
				}
				return nil, true
			}
		},
		len:      p.len,
		valLim:   p.valLim,
		skip:     p.skip,
		parallel: p.parallel,
	}
}

// Filter leaves only items of an underlying slice where fn(d[i]) is true
func (p *Pipe[T]) Filter(fn func(T) bool) *Pipe[T] {
	return &Pipe[T]{
		fn: func() func(i int) (*T, bool) {
			return func(i int) (*T, bool) {
				if obj, skipped := p.fn()(i); !skipped {
					if !fn(*obj) {
						p.skip(i)
						return nil, true
					}
					return obj, false
				}
				return nil, true
			}
		},
		len:      p.len,
		valLim:   p.valLim,
		skip:     p.skip,
		parallel: p.parallel,
	}
}

// Sort sorts the underlying slice
// TO BE IMPLEMENTED
// func (p *Pipe[T]) Sort(less func(T, T) bool) *Pipe[T] {
// 	return &Pipe[T]{
// 		fn: func() ([]T, []bool) {
// 			data, skip := p.fn()
// 			filtered := make([]T, 0, len(data)-*p.skipped)
// 			for i := range data {
// 				if !skip[i] {
// 					filtered = append(filtered, data[i])
// 				}
// 			}
// 			sort.Slice(
// 				filtered,
// 				func(i, j int) bool {
// 					return less(filtered[i], filtered[j])
// 				},
// 			)
// 			*p.skipped = 0
// 			return filtered, make([]bool, len(filtered))
// 		},
// 		skipped:    p.skipped,
// 		infinitSeq: p.infinitSeq,
// 	}
// }

// Reduce applies the result of a function to each element one-by-one
func (p *Pipe[T]) Reduce(fn func(T, T) T) *T {
	data := p.Do()
	if len(data) == 0 {
		return nil
	}
	res := data[0]
	for i := range data[1:] {
		res = fn(res, data[i+1])
	}
	return &res
}

// Get set the amount of values expected to be in result slice
// Applied only the first Gen() or Get() function in the pipe
func (p *Pipe[T]) Get(n int) *Pipe[T] {
	if n < 0 || *p.valLim != 0 || *p.len != -1 {
		return p
	}
	valLim := int64(n)
	p.valLim = &valLim
	return p
}

// Gen set the amount of values to generate as initial array
// Applied only the first Gen() or Get() function in the pipe
func (p *Pipe[T]) Gen(n int) *Pipe[T] {
	if n < 0 || *p.len != -1 || *p.valLim != 0 {
		return p
	}
	length := int64(n)
	p.len = &length
	return p
}

func (p *Pipe[T]) doToLimit() []T {
	pfn := p.fn()
	res := make([]T, 0, *p.valLim)
	for i := 0; int64(len(res)) < *p.valLim; i++ {
		obj, skipped := pfn(i)
		if !skipped {
			res = append(res, *obj)
		}

		if i == math.MaxInt {
			// TODO: should we panic here?
			break
		}
	}
	return res
}

type ev[T any] struct {
	obj     *T
	skipped bool
}

func (p *Pipe[T]) do(needResult bool) ([]T, int) {
	if *p.len == -1 && *p.valLim == 0 {
		return []T{}, 0
	}

	if *p.valLim != 0 {
		res := p.doToLimit()
		return res, len(res)
	}

	var skipCnt atomic.Int64
	var res []T
	var evals []ev[T]
	if needResult {
		res = make([]T, 0, *p.len)
		evals = make([]ev[T], *p.len)
	}

	wrks := make(chan struct{}, p.parallel)
	for i := 0; i < p.parallel; i++ {
		wrks <- struct{}{}
	}
	var wg sync.WaitGroup

	pfn := p.fn()
	wg.Add(int(*p.len))
	for i := 0; i < int(*p.len); i++ {
		<-wrks
		go func(i int) {
			defer func() {
				wrks <- struct{}{}
				wg.Done()
			}()

			obj, skipped := pfn(i)
			if skipped {
				skipCnt.Add(1)
			}
			if needResult {
				evals[i] = ev[T]{obj, skipped}
			}
		}(i)
	}
	wg.Wait()

	if needResult {
		for _, ev := range evals {
			if !ev.skipped {
				res = append(res, *ev.obj)
			}
		}
	}

	return res, int(*p.len - skipCnt.Load())
}

// Parallel set n - the amount of goroutines to run on. The value by defalut is 4
// Only the first Parallel() call is not ignored
func (p *Pipe[T]) Parallel(n int) *Pipe[T] {
	if n < 1 {
		return p
	}
	if n > maxParallelWrks {
		n = maxParallelWrks
	}
	p.parallel = n
	return p
}

// Do evaluates all the pipeline and returns the result slice
func (p *Pipe[T]) Do() []T {
	res, _ := p.do(true)
	return res
}

// Count evaluates all the pipeline and returns the amount of left items
func (p *Pipe[T]) Count() int {
	if *p.valLim != 0 {
		return int(*p.valLim)
	}
	_, cnt := p.do(false)
	return cnt
}

// func reduceSkipped[T any](data []T, skip []bool, skipped int) ([]T, []bool, int) {
// 	if skipped > len(data)/2 {
// 		res := make([]T, 0, len(data)-skipped)
// 		for i := range skip {
// 			if !skip[i] {
// 				res = append(res, data[i])
// 			}
// 		}
// 		return res, make([]bool, len(res)), 0
// 	}
// 	return data, skip, skipped
// }
