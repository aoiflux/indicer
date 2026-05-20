package structs

// ChonkMetadata stores chunk storage location and size information.
type ChonkMetadata struct {
	Path         string `msgpack:"path"`
	Offset       int64  `msgpack:"offset"`
	OriginalSize int64  `msgpack:"original_size"`
	StoredSize   int64  `msgpack:"stored_size"`
	EncodedSize  int64  `msgpack:"encoded_size"`
	Container    bool   `msgpack:"container"`
}
