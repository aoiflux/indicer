package structs

type node struct {
	key  []byte
	data []byte
}

func (n *node) setKey(key []byte) {
	n.key = key
}
func (n *node) setData(data []byte) {
	n.data = data
}
func (n node) getKey() []byte {
	return n.key
}
func (n node) getData() []byte {
	return n.data
}

type relNode struct {
	node
	index int64
}

func (r *relNode) setIndex(index int64) {
	r.index = index
}
func (r relNode) getIndex() int64 {
	return r.index
}
