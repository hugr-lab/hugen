package hub

import "github.com/hugr-lab/query-engine/types"

type Source struct {
	qe types.Querier
}

func New(qe types.Querier) *Source {
	return &Source{qe: qe}
}
