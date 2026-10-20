package auth

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Email delivery (email-delivery.ts) over net/smtp: <root>/email-delivery.json.

// EmailConfig is email-delivery.json.
type EmailConfig struct {
	Host           string    `json:"host"`
	Port           int       `json:"port"`
	Security       string    `json:"security"` // tls | starttls
	Authentication EmailAuth `json:"authentication"`
	From           string    `json:"from"`
}

// EmailAuth is SMTP authentication.
type EmailAuth struct {
	Kind     string `json:"kind"` // none | password
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Validate checks the TS schema rules.
func (c *EmailConfig) Validate() error {
	switch {
	case c.Host == "" || len(c.Host) > 253 || strings.TrimSpace(c.Host) != c.Host || strings.ContainsAny(c.Host, " \t\n"):
		return errors.New("host is invalid")
	case c.Port < 1 || c.Port > 65535:
		return errors.New("port is invalid")
	case c.Security != "tls" && c.Security != "starttls":
		return errors.New("security must be tls or starttls")
	case c.Authentication.Kind == "password" && (c.Authentication.Username == "" || len(c.Authentication.Username) > 512 ||
		c.Authentication.Password == "" || len(c.Authentication.Password) > 4096):
		return errors.New("authentication is invalid")
	case c.Authentication.Kind != "none" && c.Authentication.Kind != "password":
		return errors.New("authentication is invalid")
	case ValidEmail(c.From) == "" || len(c.From) > 320:
		return errors.New("from is invalid")
	}
	return nil
}

// Summary omits credentials.
func (c *EmailConfig) Summary() map[string]any {
	return map[string]any{"host": c.Host, "port": c.Port, "security": c.Security, "authentication": c.Authentication.Kind, "from": c.From}
}

func emailPath(rootDir string) string { return filepath.Join(rootDir, "email-delivery.json") }

// LoadEmail reads email-delivery.json; nil when not configured.
func LoadEmail(rootDir string) (*EmailConfig, error) {
	b, err := os.ReadFile(emailPath(rootDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("Unable to read email delivery configuration.")
	}
	var c EmailConfig
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || c.Validate() != nil {
		return nil, errors.New("Invalid email-delivery.json. Configure email delivery again with the LocalBase CLI.")
	}
	return &c, nil
}

// SaveEmail writes email-delivery.json (0600).
func SaveEmail(rootDir string, c *EmailConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return writePrivate(emailPath(rootDir), c)
}

// DisableEmail removes email-delivery.json; false if it was absent.
func DisableEmail(rootDir string) (bool, error) {
	err := os.Remove(emailPath(rootDir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// DeliveryError names why delivery failed without the server's text.
type DeliveryError struct{ Category string }

func (e *DeliveryError) Error() string { return "Email delivery failed (" + e.Category + ")." }

func categorize(err error) error {
	var dns *net.DNSError
	var ne net.Error
	var tlsErr *tls.RecordHeaderError
	msg := strings.ToLower(err.Error())
	switch {
	case errors.As(err, &dns):
		return &DeliveryError{"dns"}
	case errors.As(err, &ne) && ne.Timeout():
		return &DeliveryError{"timeout"}
	case errors.As(err, &tlsErr), strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"):
		return &DeliveryError{"tls"}
	case strings.HasPrefix(msg, "535") || strings.HasPrefix(msg, "534") || strings.Contains(msg, "auth"):
		return &DeliveryError{"authentication"}
	case strings.HasPrefix(msg, "55") || strings.HasPrefix(msg, "50"):
		return &DeliveryError{"recipient"}
	case strings.Contains(msg, "connect") || strings.Contains(msg, "refused") || strings.Contains(msg, "reset"):
		return &DeliveryError{"connect"}
	}
	return &DeliveryError{"unknown"}
}

// SendEmail delivers a plain-text message.
func SendEmail(c *EmailConfig, to, subject, text string) error {
	if ValidEmail(to) == "" || subject == "" || len(subject) > 200 || text == "" || len(text) > 100_000 {
		return errors.New("invalid email")
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tlsCfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	var err error
	if c.Security == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return categorize(err)
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return categorize(err)
	}
	defer client.Close()
	if c.Security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return &DeliveryError{"tls"}
		}
		if err := client.StartTLS(tlsCfg); err != nil {
			return &DeliveryError{"tls"}
		}
	}
	if c.Authentication.Kind == "password" {
		if err := client.Auth(smtp.PlainAuth("", c.Authentication.Username, c.Authentication.Password, c.Host)); err != nil {
			return &DeliveryError{"authentication"}
		}
	}
	if err := client.Mail(c.From); err != nil {
		return categorize(err)
	}
	if err := client.Rcpt(to); err != nil {
		return &DeliveryError{"recipient"}
	}
	w, err := client.Data()
	if err != nil {
		return categorize(err)
	}
	var body bytes.Buffer
	qp := quotedprintable.NewWriter(&body)
	qp.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
	qp.Close()
	fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s",
		c.From, to, mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z), RandomToken(16), c.Host, body.String())
	if err := w.Close(); err != nil {
		return categorize(err)
	}
	return client.Quit()
}
