package structs

type NearGen struct {
	RevMap     map[int64][]string
	Err        error
	Confidence float64
}
