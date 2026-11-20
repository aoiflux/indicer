package core

// Ranges renders content extents as the legacy absolute ranges: data as
// image ranges, holes and unwritten extents as sparse ranges of their length.
// FileRecord.Fragments keeps this form until W6 stores extents.
func (c Content) Ranges() []AbsRange {
	out := make([]AbsRange, 0, len(c.Extents))
	for _, e := range c.Extents {
		if e.Kind == ExtentData {
			out = append(out, AbsRange{Start: e.ImageOffset, End: e.ImageOffset + e.Length - 1})
		} else {
			out = append(out, AbsRange{Start: 0, End: e.Length - 1, Sparse: true})
		}
	}
	return out
}
