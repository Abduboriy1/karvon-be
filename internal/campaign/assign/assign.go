// Package assign picks the email variant a contact receives at a given step.
//
// The choice is deterministic: a seed derived from the campaign, the contact, the
// step and the version of the weights is mapped onto the weighted variants, so the
// same inputs always yield the same variant no matter which process asks, and the
// split converges on the configured weights across many contacts.
package assign

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"

	"github.com/google/uuid"
)

// Weighted is a variant and its share of the traffic, in percent.
type Weighted struct {
	ID     uuid.UUID
	Weight int
}

// Seed derives the assignment seed: the SHA-256 of
// campaignID + "|" + contactID + "|" + step + "|" + weightsVersion.
func Seed(campaignID, contactID uuid.UUID, step, weightsVersion int) [32]byte {
	return sha256.Sum256([]byte(campaignID.String() + "|" + contactID.String() + "|" +
		strconv.Itoa(step) + "|" + strconv.Itoa(weightsVersion)))
}

// SeedHex renders a seed as lower-case hex, for storing next to the assignment.
func SeedHex(seed [32]byte) string {
	return hex.EncodeToString(seed[:])
}

// Pick maps the first 8 bytes of the seed (big-endian) onto [0, total weight) and
// walks the variants sorted by ID, so the result does not depend on input order.
// Weights are optional: when none is positive every variant counts equally, so
// a step with variants attached always has something to hand out. Otherwise a
// variant with a zero or negative weight is never picked. Returns false only
// when there are no variants at all.
func Pick(seed [32]byte, variants []Weighted) (uuid.UUID, bool) {
	if len(variants) == 0 {
		return uuid.Nil, false
	}
	ordered := make([]Weighted, 0, len(variants))
	total := 0
	for _, v := range variants {
		if v.Weight <= 0 {
			continue
		}
		ordered = append(ordered, v)
		total += v.Weight
	}
	if total <= 0 {
		ordered = ordered[:0]
		for _, v := range variants {
			ordered = append(ordered, Weighted{ID: v.ID, Weight: 1})
		}
		total = len(ordered)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return bytes.Compare(ordered[i].ID[:], ordered[j].ID[:]) < 0
	})

	point := binary.BigEndian.Uint64(seed[:8]) % uint64(total)
	acc := uint64(0)
	for _, v := range ordered {
		if v.Weight <= 0 {
			continue
		}
		acc += uint64(v.Weight)
		if point < acc {
			return v.ID, true
		}
	}
	return ordered[len(ordered)-1].ID, true
}

// ValidateWeights checks a set of variant weights: every weight must be within
// [0, 100]. Weights are optional relative shares — Pick splits evenly when none
// is positive — so neither their total nor a positive one is required. The error
// message is suitable for a 422 response.
func ValidateWeights(weights []int) error {
	for _, w := range weights {
		if w < 0 || w > 100 {
			return errors.New("each variant weight must be between 0 and 100")
		}
	}
	return nil
}
