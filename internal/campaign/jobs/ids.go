package jobs

import (
	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/ids"
)

func newID() uuid.UUID { return ids.New() }
