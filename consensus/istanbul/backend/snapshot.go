// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package backend

import (
	"bytes"
	"encoding/json"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/istanbul"
	"github.com/ethereum/go-ethereum/consensus/istanbul/validator"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

const (
	dbKeySnapshotPrefix = "istanbul-snapshot"
)

// Vote represents a single vote that an authorized validator made to modify the
// list of authorizations.
type Vote struct {
	Validator common.Address `json:"validator"` // Authorized validator that cast this vote
	Block     uint64         `json:"block"`     // Block number the vote was cast in (expire old votes)
	Address   common.Address `json:"address"`   // Account being voted on to change its authorization
	Authorize bool           `json:"authorize"` // Whether to authorize or deauthorize the voted account
}

// Tally is a simple vote tally to keep the current score of votes. Votes that
// go against the proposal aren't counted since it's equivalent to not voting.
type Tally struct {
	Authorize bool `json:"authorize"` // Whether the vote it about authorizing or kicking someone
	Votes     int  `json:"votes"`     // Number of votes until now wanting to pass the proposal
}

// Snapshot is the state of the authorization voting at a given point in time.
type Snapshot struct {
	Epoch uint64 // The number of blocks after which to checkpoint and reset the pending votes

	Number uint64                   // Block number where the snapshot was created
	Hash   common.Hash              // Block hash where the snapshot was created
	Votes  []*Vote                  // List of votes cast in chronological order
	Tally  map[common.Address]Tally // Current vote tally to avoid recalculating
	ValSet istanbul.ValidatorSet    // Set of authorized validators at this moment
}

// newSnapshot create a new snapshot with the specified startup parameters. This
// method does not initialize the set of recent validators, so only ever use if for
// the genesis block.
func newSnapshot(epoch uint64, number uint64, hash common.Hash, valSet istanbul.ValidatorSet) *Snapshot {
	snap := &Snapshot{
		Epoch:  epoch,
		Number: number,
		Hash:   hash,
		ValSet: valSet,
		Tally:  make(map[common.Address]Tally),
	}
	return snap
}

// loadSnapshot loads an existing snapshot from the database.
func loadSnapshot(epoch uint64, db ethdb.Database, hash common.Hash) (*Snapshot, error) {
	blob, err := db.Get(append([]byte(dbKeySnapshotPrefix), hash[:]...))
	if err != nil {
		return nil, err
	}
	snap := new(Snapshot)
	if err := json.Unmarshal(blob, snap); err != nil {
		return nil, err
	}
	snap.Epoch = epoch

	return snap, nil
}

// store inserts the snapshot into the database.
func (s *Snapshot) store(db ethdb.Database) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return db.Put(append([]byte(dbKeySnapshotPrefix), s.Hash[:]...), blob)
}

// copy creates a deep copy of the snapshot, though not the individual votes.
func (s *Snapshot) copy() *Snapshot {
	cpy := &Snapshot{
		Epoch:  s.Epoch,
		Number: s.Number,
		Hash:   s.Hash,
		ValSet: s.ValSet.Copy(),
		Votes:  make([]*Vote, len(s.Votes)),
		Tally:  make(map[common.Address]Tally),
	}

	for address, tally := range s.Tally {
		cpy.Tally[address] = tally
	}
	copy(cpy.Votes, s.Votes)

	return cpy
}

// checkVote return whether it's a valid vote
func (s *Snapshot) checkVote(address common.Address, authorize bool) bool {
	_, validator := s.ValSet.GetByAddress(address)
	return (validator != nil && !authorize) || (validator == nil && authorize)
}

// cast adds a new vote into the tally.
func (s *Snapshot) cast(address common.Address, authorize bool) bool {
	// Ensure the vote is meaningful
	if !s.checkVote(address, authorize) {
		return false
	}
	// Cast the vote into an existing or new tally
	if old, ok := s.Tally[address]; ok {
		old.Votes++
		s.Tally[address] = old
	} else {
		s.Tally[address] = Tally{Authorize: authorize, Votes: 1}
	}
	return true
}

// uncast removes a previously cast vote from the tally.
func (s *Snapshot) uncast(address common.Address, authorize bool) bool {
	// If there's no tally, it's a dangling vote, just drop
	tally, ok := s.Tally[address]
	if !ok {
		return false
	}
	// Ensure we only revert counted votes
	if tally.Authorize != authorize {
		return false
	}
	// Otherwise revert the vote
	if tally.Votes > 1 {
		tally.Votes--
		s.Tally[address] = tally
	} else {
		delete(s.Tally, address)
	}
	return true
}

// apply creates a new authorization snapshot by applying the given headers to
// the original one.
func (s *Snapshot) apply(headers []*types.Header, isQBFTConsensus bool, qbftBlockNumber int64) (*Snapshot, error) {
	// Allow passing in no headers for cleaner code
	if len(headers) == 0 {
		return s, nil
	}
	// Sanity check that the headers can be applied
	for i := 0; i < len(headers)-1; i++ {
		if headers[i+1].Number.Uint64() != headers[i].Number.Uint64()+1 {
			return nil, errInvalidVotingChain
		}
	}
	if headers[0].Number.Uint64() != s.Number+1 {
		return nil, errInvalidVotingChain
	}
	// Iterate through the headers and create a new snapshot
	snap := s.copy()

	for _, header := range headers {
		if isQBFTConsensus && header.Number.Int64() > qbftBlockNumber {
			err := snap.qbftApply(header)
			if err != nil {
				return nil, err
			}
		} else {
			err := snap.legacyApply(header)
			if err != nil {
				return nil, err
			}
		}
	}
	snap.Number += uint64(len(headers))
	snap.Hash = headers[len(headers)-1].Hash()

	return snap, nil
}

func (s *Snapshot) legacyApply(header *types.Header) error {
	// Remove any votes on checkpoint blocks
	number := header.Number.Uint64()
	if number%s.Epoch == 0 {
		s.Votes = nil
		s.Tally = make(map[common.Address]Tally)
	}
	// Resolve the authorization key and check against validators
	validator, err := ecrecoverFromSignedHeader(header)
	if err != nil {
		return err
	}
	if _, v := s.ValSet.GetByAddress(validator); v == nil {
		return errUnauthorized
	}

	// Header authorized, discard any previous votes from the validator
	for i, vote := range s.Votes {
		if vote.Validator == validator && vote.Address == header.Coinbase {
			// Uncast the vote from the cached tally
			s.uncast(vote.Address, vote.Authorize)

			// Uncast the vote from the chronological list
			s.Votes = append(s.Votes[:i], s.Votes[i+1:]...)
			break // only one vote allowed
		}
	}
	// Tally up the new vote from the validator
	var authorize bool
	switch {
	case bytes.Equal(header.Nonce[:], nonceAuthVote):
		authorize = true
	case bytes.Equal(header.Nonce[:], nonceDropVote):
		authorize = false
	default:
		return errInvalidVote
	}
	if s.cast(header.Coinbase, authorize) {
		s.Votes = append(s.Votes, &Vote{
			Validator: validator,
			Block:     number,
			Address:   header.Coinbase,
			Authorize: authorize,
		})
	}
	// If the vote passed, update the list of validators
	if tally := s.Tally[header.Coinbase]; tally.Votes > s.ValSet.Size()/2 {
		if tally.Authorize {
			s.ValSet.AddValidator(header.Coinbase)
		} else {
			s.ValSet.RemoveValidator(header.Coinbase)

			// Discard any previous votes the deauthorized validator cast
			for i := 0; i < len(s.Votes); i++ {
				if s.Votes[i].Validator == header.Coinbase {
					// Uncast the vote from the cached tally
					s.uncast(s.Votes[i].Address, s.Votes[i].Authorize)

					// Uncast the vote from the chronological list
					s.Votes = append(s.Votes[:i], s.Votes[i+1:]...)

					i--
				}
			}
		}
		// Discard any previous votes around the just changed account
		for i := 0; i < len(s.Votes); i++ {
			if s.Votes[i].Address == header.Coinbase {
				s.Votes = append(s.Votes[:i], s.Votes[i+1:]...)
				i--
			}
		}
		delete(s.Tally, header.Coinbase)
	}
	return nil
}

// qbftApply creates a new authorization snapshot using qbftExtra by applying the given headers to
// the original one.
func (s *Snapshot) qbftApply(header *types.Header) error {
	// Remove any votes on checkpoint blocks
	number := header.Number.Uint64()
	if number%s.Epoch == 0 {
		s.Votes = nil
		s.Tally = make(map[common.Address]Tally)
	}
	// Resolve the authorization key and check against validators
	validator, err := ecrecoverFromCoinbase(header)
	if err != nil {
		return err
	}
	if _, v := s.ValSet.GetByAddress(validator); v == nil {
		return errUnauthorized
	}

	// Get the Vote information from header
	qbftExtra, err := types.ExtractQbftExtra(header)
	if err != nil {
		return errInvalidExtraDataFormat
	}

	var validatorVote *types.ValidatorVote
	if qbftExtra.Vote == nil {
		validatorVote = &types.ValidatorVote{RecipientAddress: common.Address{}, VoteType: types.QbftDropVote}
	} else {
		validatorVote = qbftExtra.Vote
	}

	// Header authorized, discard any previous votes from the validator
	for i, vote := range s.Votes {
		if vote.Validator == validator && vote.Address == validatorVote.RecipientAddress {
			// Uncast the vote from the cached tally
			s.uncast(vote.Address, vote.Authorize)

			// Uncast the vote from the chronological list
			s.Votes = append(s.Votes[:i], s.Votes[i+1:]...)
			break // only one vote allowed
		}
	}
	// Tally up the new vote from the validator
	var authorize bool
	switch {
	case validatorVote.VoteType == types.QbftAuthVote:
		authorize = true
	case validatorVote.VoteType == types.QbftDropVote:
		authorize = false
	default:
		return errInvalidVote
	}
	if s.cast(validatorVote.RecipientAddress, authorize) {
		s.Votes = append(s.Votes, &Vote{
			Validator: validator,
			Block:     number,
			Address:   validatorVote.RecipientAddress,
			Authorize: authorize,
		})
	}
	// If the vote passed, update the list of validators
	if tally := s.Tally[validatorVote.RecipientAddress]; tally.Votes > s.ValSet.Size()/2 {
		if tally.Authorize {
			s.ValSet.AddValidator(validatorVote.RecipientAddress)
		} else {
			s.ValSet.RemoveValidator(validatorVote.RecipientAddress)

			// Discard any previous votes the deauthorized validator cast
			for i := 0; i < len(s.Votes); i++ {
				if s.Votes[i].Validator == validatorVote.RecipientAddress {
					// Uncast the vote from the cached tally
					s.uncast(s.Votes[i].Address, s.Votes[i].Authorize)

					// Uncast the vote from the chronological list
					s.Votes = append(s.Votes[:i], s.Votes[i+1:]...)

					i--
				}
			}
		}
		// Discard any previous votes around the just changed account
		for i := 0; i < len(s.Votes); i++ {
			if s.Votes[i].Address == validatorVote.RecipientAddress {
				s.Votes = append(s.Votes[:i], s.Votes[i+1:]...)
				i--
			}
		}
		delete(s.Tally, validatorVote.RecipientAddress)
	}
	return nil
}

// validators retrieves the list of authorized validators in ascending order.
func (s *Snapshot) validators() []common.Address {
	validators := make([]common.Address, 0, s.ValSet.Size())
	for _, validator := range s.ValSet.List() {
		validators = append(validators, validator.Address())
	}
	for i := 0; i < len(validators); i++ {
		for j := i + 1; j < len(validators); j++ {
			if bytes.Compare(validators[i][:], validators[j][:]) > 0 {
				validators[i], validators[j] = validators[j], validators[i]
			}
		}
	}
	return validators
}

type snapshotJSON struct {
	Epoch  uint64                   `json:"epoch"`
	Number uint64                   `json:"number"`
	Hash   common.Hash              `json:"hash"`
	Votes  []*Vote                  `json:"votes"`
	Tally  map[common.Address]Tally `json:"tally"`

	// for validator set
	Validators []common.Address          `json:"validators"`
	Policy     istanbul.ProposerPolicyId `json:"policy"`
}

func (s *Snapshot) toJSONStruct() *snapshotJSON {
	return &snapshotJSON{
		Epoch:      s.Epoch,
		Number:     s.Number,
		Hash:       s.Hash,
		Votes:      s.Votes,
		Tally:      s.Tally,
		Validators: s.validators(),
		Policy:     s.ValSet.Policy().Id,
	}
}

// Unmarshal from a json byte array
func (s *Snapshot) UnmarshalJSON(b []byte) error {
	var j snapshotJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}

	s.Epoch = j.Epoch
	s.Number = j.Number
	s.Hash = j.Hash
	s.Votes = j.Votes
	s.Tally = j.Tally

	// Setting the By function to ValidatorSortByStringFunc should be fine, as the validator do not change only the order changes
	pp := &istanbul.ProposerPolicy{Id: j.Policy, By: istanbul.ValidatorSortByString()}
	s.ValSet = validator.NewSet(j.Validators, pp)
	return nil
}

// Marshal to a json byte array
func (s *Snapshot) MarshalJSON() ([]byte, error) {
	j := s.toJSONStruct()
	return json.Marshal(j)
}
