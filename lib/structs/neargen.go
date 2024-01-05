package structs

type NearGen struct {
	RevMap     map[string]struct{}
	Err        error
	Index      int64
	Confidence float64
}
