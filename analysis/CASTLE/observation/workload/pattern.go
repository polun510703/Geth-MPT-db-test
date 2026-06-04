// Package workload provides access-pattern generators and dataset loaders
// for the observation benchmark (analysis/CASTLE/observation).
package workload

import (
	"fmt"
	"math/rand"
)

// Pattern is a lazy index iterator over a dataset of size N.
type Pattern interface {
	Next() int
}

type uniformPattern struct {
	n   int
	rng *rand.Rand
}

func (p *uniformPattern) Next() int { return p.rng.Intn(p.n) }

type zipfPattern struct {
	z      *rand.Zipf
	offset int
}

func (p *zipfPattern) Next() int { return int(p.z.Uint64()) + p.offset }

type seqPattern struct {
	n int
	i int
}

func (p *seqPattern) Next() int {
	v := p.i % p.n
	p.i++
	return v
}

// PatternParams collects all tunables; not every field is used by every pattern.
type PatternParams struct {
	N           int     // dataset size
	Seed        int64   // rng seed
	ZipfS       float64 // zipf skew parameter (>1)
	RecentFrac  float64 // recent-heavy hot zone fraction in (0,1]
}

// NewPattern returns a lazy iterator. Supported kinds:
//   - "uniform" — uniform random in [0,N)
//   - "zipf"    — zipf over [0,N), skew=ZipfS
//   - "seq"     — sequential 0,1,2,...,N-1 (then wrap)
//   - "recent"  — zipf restricted to the trailing RecentFrac of the dataset
func NewPattern(kind string, p PatternParams) (Pattern, error) {
	if p.N <= 0 {
		return nil, fmt.Errorf("workload: N must be positive (got %d)", p.N)
	}
	rng := rand.New(rand.NewSource(p.Seed))
	switch kind {
	case "uniform":
		return &uniformPattern{n: p.N, rng: rng}, nil
	case "seq":
		return &seqPattern{n: p.N}, nil
	case "zipf":
		s := p.ZipfS
		if s <= 1.0 {
			s = 1.1
		}
		z := rand.NewZipf(rng, s, 1.0, uint64(p.N-1))
		if z == nil {
			return nil, fmt.Errorf("workload: rand.NewZipf returned nil (s=%v N=%d)", s, p.N)
		}
		return &zipfPattern{z: z}, nil
	case "recent":
		frac := p.RecentFrac
		if frac <= 0 || frac > 1 {
			frac = 0.1
		}
		hot := int(float64(p.N) * frac)
		if hot <= 0 {
			hot = 1
		}
		offset := p.N - hot
		s := p.ZipfS
		if s <= 1.0 {
			s = 1.1
		}
		z := rand.NewZipf(rng, s, 1.0, uint64(hot-1))
		if z == nil {
			return nil, fmt.Errorf("workload: rand.NewZipf returned nil for recent (s=%v hot=%d)", s, hot)
		}
		return &zipfPattern{z: z, offset: offset}, nil
	default:
		return nil, fmt.Errorf("workload: unknown pattern %q (want uniform|zipf|seq|recent)", kind)
	}
}
