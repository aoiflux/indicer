package detectors

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"

	"indicer/lib/microartefact/model"

	"www.velocidex.com/golang/evtx"
)

type EVTXDetector struct {
	MaxResults int
}

func (d EVTXDetector) Name() string {
	return "evtx-native"
}

func (d EVTXDetector) Detect(_ model.FileRecord, content []byte) ([]model.Artefact, error) {
	reader := bytes.NewReader(content)
	var header evtx.EVTXHeader
	if err := binary.Read(reader, binary.LittleEndian, &header); err != nil {
		return nil, nil
	}
	if string(header.Magic[:]) != evtx.EVTX_HEADER_MAGIC {
		return nil, nil
	}

	maxResults := resolveMaxResults(d.MaxResults, defaultEVTXResults)

	results := make([]model.Artefact, 0, maxResults)
	offset := int64(header.HeaderBlockSize)
	for offset < int64(len(content)) && len(results) < maxResults {
		chunk, err := evtx.NewChunk(bytes.NewReader(content), offset)
		if err != nil {
			break
		}
		if string(chunk.Header.Magic[:]) != evtx.EVTX_CHUNK_HEADER_MAGIC {
			break
		}

		records, err := chunk.Parse(0)
		if err != nil {
			break
		}
		for _, record := range records {
			if len(results) >= maxResults {
				break
			}
			eventText := fmt.Sprintf("%v", record.Event)
			if eventText == "<nil>" || eventText == "" {
				eventText = fmt.Sprintf("RecordID=%d", record.Header.RecordID)
			}
			results = append(results, model.Artefact{
				Kind:       "windows_event_record",
				Detector:   d.Name(),
				Value:      truncate(eventText, 512),
				Summary:    truncate(eventText, 128),
				Confidence: 0.95,
				Attributes: map[string]string{
					"record_id":    strconv.FormatUint(record.Header.RecordID, 10),
					"timestamp":    windowsFiletimeToRFC3339(record.Header.FileTime),
					"chunk_offset": strconv.FormatInt(offset, 10),
				},
			})
		}

		offset += evtx.EVTX_CHUNK_SIZE
	}

	return results, nil
}
