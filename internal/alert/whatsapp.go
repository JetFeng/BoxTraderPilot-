//go:build whatsapp

// WhatsApp alerting via whatsmeow (personal-account automation).
//
// To enable, build with -tags whatsapp. On first Connect the account is
// paired by scanning a QR code printed to the terminal; the session is
// stored in DBPath and reused afterwards.
//
// Note: whatsmeow automates a personal WhatsApp account over the
// multi-device protocol. This is against WhatsApp's terms of service;
// accounts can be restricted or banned. Use at your own risk, keep
// volume low, and consider a secondary number.
package alert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	// Pure-Go SQLite driver (registers the "sqlite" driver name).
	_ "modernc.org/sqlite"
)

// WhatsApp delivers alerts through a paired personal WhatsApp account.
type WhatsApp struct {
	cfg WhatsAppConfig
	out io.Writer // QR and pairing output

	cli  *whatsmeow.Client
	self *types.JID

	mu           sync.Mutex
	disconnected bool
}

// NewWhatsApp opens the session store and prepares a client. The
// account is not connected until Connect.
func NewWhatsApp(cfg WhatsAppConfig) (*WhatsApp, error) {
	if cfg.DBPath == "" {
		return nil, errors.New("whatsapp: DBPath is required")
	}
	dbLog := waLog.Stdout("WhatsApp DB", "ERROR", false)
	container, err := sqlstore.New(context.Background(), "sqlite", "file:"+cfg.DBPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		return nil, fmt.Errorf("whatsapp session store: %w", err)
	}
	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("whatsapp device store: %w", err)
	}
	cliLog := waLog.Stdout("WhatsApp", "ERROR", false)
	return &WhatsApp{cfg: cfg, out: os.Stdout, cli: whatsmeow.NewClient(device, cliLog)}, nil
}

// Connect logs in (QR pairing on first use) and starts a background
// reconnect loop tied to ctx. It blocks until pairing succeeds or ctx
// is canceled.
func (w *WhatsApp) Connect(ctx context.Context) error {
	w.cli.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Disconnected); ok {
			w.mu.Lock()
			w.disconnected = true
			w.mu.Unlock()
		}
	})

	if w.cli.Store.ID == nil {
		if err := w.pair(ctx); err != nil {
			return err
		}
	} else if err := w.cli.Connect(); err != nil {
		return fmt.Errorf("whatsapp connect: %w", err)
	}
	w.mu.Lock()
	w.self = w.cli.Store.ID
	w.disconnected = false
	w.mu.Unlock()

	go w.reconnectLoop(ctx)
	return nil
}

// pair runs the QR-code login flow.
func (w *WhatsApp) pair(ctx context.Context) error {
	qrChan, err := w.cli.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("whatsapp qr channel: %w", err)
	}
	if err := w.cli.Connect(); err != nil {
		return fmt.Errorf("whatsapp connect: %w", err)
	}
	fmt.Fprintln(w.out, "WhatsApp: no session found. Open WhatsApp on your phone → Settings → Linked Devices → Link a Device, and scan this QR code:")
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("whatsapp pairing aborted: %w", ctx.Err())
		case evt, ok := <-qrChan:
			if !ok {
				return errors.New("whatsapp qr channel closed before pairing completed")
			}
			switch evt.Event {
			case "code":
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, w.out)
			case "success":
				fmt.Fprintln(w.out, "WhatsApp: paired.")
				return nil
			default:
				fmt.Fprintf(w.out, "WhatsApp: login event %q\n", evt.Event)
			}
		}
	}
}

// reconnectLoop re-establishes the connection after server-side drops.
func (w *WhatsApp) reconnectLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.mu.Lock()
		dropped := w.disconnected
		w.disconnected = false
		w.mu.Unlock()
		if !dropped {
			continue
		}
		if err := w.cli.Connect(); err != nil {
			w.mu.Lock()
			w.disconnected = true
			w.mu.Unlock()
		}
	}
}

// recipientJID resolves the destination: configured recipient, or the
// paired account itself.
func (w *WhatsApp) recipientJID() (types.JID, error) {
	if w.cfg.Recipient != "" {
		digits, err := ParseE164(w.cfg.Recipient)
		if err != nil {
			return types.JID{}, err
		}
		return types.NewJID(digits, types.DefaultUserServer), nil
	}
	w.mu.Lock()
	self := w.self
	w.mu.Unlock()
	if self == nil {
		return types.JID{}, errors.New("whatsapp: not connected")
	}
	return *self, nil
}

// send delivers one text message, connecting lazily if needed.
func (w *WhatsApp) send(ctx context.Context, text string) error {
	if !w.cli.IsConnected() {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := w.Connect(cctx)
		cancel()
		if err != nil {
			return err
		}
	}
	to, err := w.recipientJID()
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = w.cli.SendMessage(sctx, to, &waE2E.Message{Conversation: proto.String(text)})
	return err
}

// Notify implements Alerter.
func (w *WhatsApp) Notify(ctx context.Context, o Opportunity) error {
	return w.send(ctx, o.Text())
}

// NotifyStale implements Alerter.
func (w *WhatsApp) NotifyStale(ctx context.Context, age, limit time.Duration) error {
	return w.send(ctx, StaleText(age, limit))
}

// Close disconnects the client.
func (w *WhatsApp) Close() error {
	w.cli.Disconnect()
	return nil
}
