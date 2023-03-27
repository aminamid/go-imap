package imapserver

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *conn) handleNamespace(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	data, err := c.session.Namespace()
	if err != nil {
		return err
	}

	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("NAMESPACE").SP()
	writeNamespace(enc.Encoder, data.Personal)
	enc.SP()
	writeNamespace(enc.Encoder, data.Other)
	enc.SP()
	writeNamespace(enc.Encoder, data.Shared)
	return enc.CRLF()
}

func writeNamespace(enc *imapwire.Encoder, l []imap.NamespaceDescriptor) {
	if l == nil {
		enc.NIL()
		return
	}

	enc.List(len(l), func(i int) {
		descr := l[i]
		enc.Special('(').String(descr.Prefix).SP()
		if descr.Delim == 0 {
			enc.NIL()
		} else {
			enc.String(string(descr.Delim)) // TODO: ensure we always use DQUOTE QUOTED-CHAR DQUOTE
		}
		enc.Special(')')
	})
}