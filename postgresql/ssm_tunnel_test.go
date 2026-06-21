package postgresql

import "testing"

// TestSSMDataSequencing locks in the data-channel sequencing: the port-forwarding
// data stream starts at sequence 0 with the SYN flag and increments from there.
// The handshake response is sent outside this sequence (the agent does not count
// it toward the expected data sequence), so the first data message must be 0 —
// starting at 1 makes the agent buffer the data as out-of-order until it times
// out.
func TestSSMInputSequencing(t *testing.T) {
	dc := &ssmDataChannel{}

	// The handshake response is the first client message: sequence 0 with SYN.
	hs := dc.nextInput(smPayloadTypeHandshakeResponse, []byte("handshake"))
	if hs.SequenceNumber != 0 || hs.Flags != flagSyn {
		t.Fatalf("handshake response: seq=%d flags=%d, want seq=0 flags=%d", hs.SequenceNumber, hs.Flags, flagSyn)
	}

	// smux data frames follow contiguously at sequence 1, 2, ... with no SYN.
	for i := int64(1); i <= 3; i++ {
		d := dc.nextInput(smPayloadTypeOutput, []byte("x"))
		if d.SequenceNumber != i {
			t.Fatalf("data message: sequence = %d, want %d", d.SequenceNumber, i)
		}
		if d.Flags != 0 {
			t.Fatalf("data message %d: flags = %d, want 0 (SYN only on the first)", i, d.Flags)
		}
	}
}
