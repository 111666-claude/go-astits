package astits

// elementaryStreamInfo represents the PMT properties of an elementary stream.
type elementaryStreamInfo struct {
	programNumber uint16
	streamType    StreamType
}

// elementaryStreamMap represents an elementary stream ids map
type elementaryStreamMap struct {
	// We use map[uint32] instead map[uint16] as go runtime provide optimized hash functions for (u)int32/64 keys
	es map[uint32]elementaryStreamInfo // Indexed by PID
}

// newElementaryStreamMap creates a new elementary stream ids map
func newElementaryStreamMap() *elementaryStreamMap {
	return &elementaryStreamMap{
		es: make(map[uint32]elementaryStreamInfo),
	}
}

// setUnlocked sets the properties of an elementary stream
func (m elementaryStreamMap) setUnlocked(pid, number uint16) {
	m.es[uint32(pid)] = elementaryStreamInfo{programNumber: number}
}

// setStreamUnlocked sets the PMT stream type attached to a PID
func (m elementaryStreamMap) setStreamUnlocked(pid, number uint16, streamType StreamType) {
	m.es[uint32(pid)] = elementaryStreamInfo{programNumber: number, streamType: streamType}
}

// existsUnlocked checks whether the stream with this pid exists
func (m elementaryStreamMap) existsUnlocked(pid uint16) (ok bool) {
	_, ok = m.es[uint32(pid)]
	return
}

// streamTypeUnlocked returns the PMT stream type attached to a PID
func (m elementaryStreamMap) streamTypeUnlocked(pid uint16) (streamType StreamType, ok bool) {
	var info elementaryStreamInfo
	info, ok = m.es[uint32(pid)]
	return info.streamType, ok
}

func (m elementaryStreamMap) unsetUnlocked(pid uint16) {
	delete(m.es, uint32(pid))
}
