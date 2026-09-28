package cloudauth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTP sends codes over implicit TLS (465) or required STARTTLS (587).
// Credentials and sender configuration are supplied through Kubernetes Secret
// environment variables; they are never written into the application state.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

func (m SMTP) SendCode(ctx context.Context, to, code string) error {
	if len(code) != 8 {
		return errors.New("invalid sign-in code")
	}
	return m.send(ctx, to, "Your Warden sign-in code", "Your Warden sign-in code is "+code+".\r\n\r\nIt expires in 10 minutes. If you did not request it, you can ignore this email.\r\n")
}

func (m SMTP) SendInvitation(ctx context.Context, to, organization, role, origin string) error {
	return m.send(ctx, to, "You're invited to Warden", invitationText(to, organization, role, origin))
}

func invitationText(to, organization, role, origin string) string {
	access := "a member"
	if role == "admin" {
		access = "an organization administrator"
	}
	return fmt.Sprintf("You've been added to %s on Warden as %s.\r\n\r\nSign in at %s using %s. We will email you a one-time sign-in code.\r\n\r\nSelect %s after signing in. To connect recording hardware, open Recordings, then Manage devices. Register a device to get a key and a ready-to-run Python script.\r\n\r\nAPI guide: %s/recording-api/\r\n", organization, access, origin, to, organization, strings.TrimRight(origin, "/"))
}

func (m SMTP) send(ctx context.Context, to, subject, message string) error {
	if m.Host == "" || (m.Port != 465 && m.Port != 587) || m.Username == "" || m.Password == "" {
		return errors.New("SMTP host, port 465 or 587, username and password required")
	}
	from, err := NormalizeEmail(m.From)
	if err != nil {
		return fmt.Errorf("SMTP sender: %w", err)
	}
	to, err = NormalizeEmail(to)
	if err != nil {
		return err
	}
	address := net.JoinHostPort(m.Host, strconv.Itoa(m.Port))
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	if m.Port == 465 {
		conn, err = tls.DialWithDialer(&dialer, "tcp", address, &tls.Config{ServerName: m.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	client, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if m.Port == 587 {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server does not support STARTTLS")
		}
		if err = client.StartTLS(&tls.Config{ServerName: m.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if err = client.Auth(smtp.PlainAuth("", m.Username, m.Password, m.Host)); err != nil {
		return err
	}
	if err = client.Mail(from); err != nil {
		return err
	}
	if err = client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	body := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		message,
	}, "\r\n")
	if _, err = w.Write([]byte(body)); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return client.Quit()
}
