package torrenttransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

var ErrPriorAdd = errors.New("transfer add already claimed; consult original receipt")

// AddIntent binds the approved destination and exact transport copy. The caller
// provides a fresh scope/source check to run after the protocol's preflight.
type AddIntent struct {
	Ledger        *Ledger
	Receipt       string
	ScopeProof    string
	Receiver      string
	WireID        string
	Directory     string
	ContentDigest string
	Revalidate    func(context.Context) error
}

func (i AddIntent) Validate() error {
	if i.Ledger == nil || i.Revalidate == nil || !Hex(i.Receipt, 64) || !Hex(i.ScopeProof, 64) || !Hex(i.Receiver, 64) || !Hex(i.WireID, 40) || !Hex(i.ContentDigest, 64) {
		return ErrState
	}
	dir, err := NormalizeRemote(i.Directory)
	if err != nil || dir != i.Directory {
		return ErrState
	}
	return nil
}

// Send owns the interval between protocol preflight and the actual add RPC.
// A previous intent is never replayed, even if its response was acknowledged.
// The controller can look up the first receipt using this same action key.
func (i AddIntent) Send(ctx context.Context, address, id, dir string, content []byte, send func() (AddReceipt, error)) (AddReceipt, error) {
	if err := i.Validate(); err != nil {
		return AddReceipt{}, err
	}
	digest := sha256.Sum256(content)
	if send == nil || Hash(address) != i.Receiver || id != i.WireID || dir != i.Directory || hex.EncodeToString(digest[:]) != i.ContentDigest {
		return AddReceipt{}, ErrState
	}
	if err := i.Revalidate(ctx); err != nil {
		return AddReceipt{}, err
	}
	key := ActionKey{Receiver: i.Receiver, WireID: id, Action: "add"}
	proof := Hash([]string{i.ScopeProof, dir, i.ContentDigest})
	_, won, err := i.Ledger.Claim(ctx, key, i.Receipt, proof)
	if err != nil {
		return AddReceipt{}, err
	}
	if !won {
		return AddReceipt{}, ErrPriorAdd
	}
	reply, sendErr := send()
	if sendErr == nil && reply.WireID != id {
		sendErr = ErrState
	}
	state := "acknowledged"
	if sendErr != nil {
		state = "unknown"
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var evidence *AddReceipt
	if sendErr == nil {
		evidence = &reply
	}
	if err := i.Ledger.finish(save, key, i.Receipt, proof, state, evidence); err != nil {
		return AddReceipt{}, errors.Join(sendErr, err)
	}
	return reply, sendErr
}
