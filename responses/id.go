package responses

import (
	"github.com/aminamid/go-imap"
	"github.com/aminamid/go-imap/id"
)

type Id struct {
	ID id.ID
}

func (r *Id) Handle(resp imap.Resp) (err error) {
	name, fields, ok := imap.ParseNamedResp(resp)
	if !ok || name != "ID" {
		return ErrUnhandled
	}

	r.ID, err = id.ParseID(fields)

	return
}

func (r *Id) Parse(fields []interface{}) (err error) {
	r.ID, err = id.ParseID(fields)
	return
}

func (r *Id) WriteTo(w *imap.Writer) error {
	fields := []interface{}{imap.RawString("ID"), id.FormatID(r.ID)}

	res := imap.NewUntaggedResp(fields)
	return res.WriteTo(w)
}
