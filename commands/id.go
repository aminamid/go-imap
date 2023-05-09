package commands

import (
	"github.com/aminamid/go-imap"
	"github.com/aminamid/go-imap/id"
)

// An ID command.
// See RFC 2971 section 3.1.
type Id struct {
	ID id.ID
}

func (cmd *Id) Command() *imap.Command {
	return &imap.Command{
		Name:      "ID",
		Arguments: []interface{}{id.FormatID(cmd.ID)},
	}
}

func (cmd *Id) Parse(fields []interface{}) (err error) {
	cmd.ID, err = id.ParseID(fields)
	return
}
