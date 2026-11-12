package service

import (
	"fmt"
	"log"
	"net/smtp"
	"strings"
)

// Mailer отвечает за отправку email через SMTP
type Mailer struct {
	host         string
	port         int
	user         string
	pass         string
	fromEmail    string
	fromName     string
	webURL       string
}

// NewMailer создаёт почтовый сервис из конфига
func NewMailer(smtpHost string, smtpPort int, smtpUser string, smtpPass string, smtpFromEmail string, smtpFromName string, webURL string) *Mailer {
	return &Mailer{
		host:        smtpHost,
		port:        smtpPort,
		user:        smtpUser,
		pass:        smtpPass,
		fromEmail:   smtpFromEmail,
		fromName:    smtpFromName,
		webURL:      webURL,
	}
}

// SendVerification отправляет письмо с ссылкой подтверждения регистрации
func (m *Mailer) SendVerification(toEmail string, verifyToken string) error {
	if m.host == "" || m.user == "" || m.pass == "" {
		log.Printf("[MAILER] SMTP не настроен — лог для проверки: verification token для %s, ссылка: %s/auth.html?verify=%s",
			toEmail, m.webURL, verifyToken)
		return nil
	}

	verifyURL := fmt.Sprintf("%s/auth.html?verify=%s", m.webURL, verifyToken)

	subject := "Подтверждение регистрации на WB-Bidder"
	fromName := m.fromName
	if fromName == "" {
		fromName = "WB-Bidder"
	}

	body := m.buildEmailBody(fromName, verifyURL)

	msg := []byte(fmt.Sprintf(
		"From: %s <%s>\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"Content-Type: text/html; charset=UTF-8\r\n\r\n"+
			"%s",
		fromName, m.fromEmail, toEmail, subject, body,
	))

	addr := fmt.Sprintf("%s:%d", m.host, m.port)
	auth := smtp.PlainAuth("", m.user, m.pass, m.host)

	err := smtp.SendMail(addr, auth, m.fromEmail, []string{toEmail}, msg)
	if err != nil {
		log.Printf("[MAILER ОШИБКА] Не удалось отправить письмо на %s: %v", toEmail, err)
		return fmt.Errorf("отправка письма: %w", err)
	}

	log.Printf("[MAILER] Письмо подтверждения отправлено на %s", toEmail)
	return nil
}

// SendReset отправляет письмо со ссылкой сброса пароля
func (m *Mailer) SendReset(toEmail string, resetToken string) error {
	if m.host == "" || m.user == "" || m.pass == "" {
		log.Printf("[MAILER] SMTP не настроен — лог для проверки: reset token для %s, ссылка: %s/reset.html?token=%s",
			toEmail, m.webURL, resetToken)
		return nil
	}

	resetURL := fmt.Sprintf("%s/reset.html?token=%s", m.webURL, resetToken)

	subject := "Сброс пароля на WB-Bidder"
	fromName := m.fromName
	if fromName == "" {
		fromName = "WB-Bidder"
	}

	body := m.buildResetBody(fromName, resetURL)

	msg := []byte(fmt.Sprintf(
		"From: %s <%s>\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"Content-Type: text/html; charset=UTF-8\r\n\r\n"+
			"%s",
		fromName, m.fromEmail, toEmail, subject, body,
	))

	addr := fmt.Sprintf("%s:%d", m.host, m.port)
	auth := smtp.PlainAuth("", m.user, m.pass, m.host)

	err := smtp.SendMail(addr, auth, m.fromEmail, []string{toEmail}, msg)
	if err != nil {
		log.Printf("[MAILER ОШИБКА] Не удалось отправить письмо сброса на %s: %v", toEmail, err)
		return fmt.Errorf("отправка письма сброса: %w", err)
	}

	log.Printf("[MAILER] Письмо сброса отправлено на %s", toEmail)
	return nil
}

func (m *Mailer) buildEmailBody(fromName string, verifyURL string) string {
	return fmt.Sprintf(`
		<div style="max-width: 500px; margin: 0 auto; font-family: Arial, sans-serif; padding: 20px;">
			<h2 style="color: #6b21a8;">Добро пожаловать в %s!</h2>
			<p>Спасибо за регистрацию. Нажмите кнопку ниже, чтобы подтвердить свой email:</p>
			<p style="text-align: center; margin: 30px 0;">
				<a href="%s" 
				   style="background-color: #6b21a8; color: white; padding: 14px 32px; 
						  text-decoration: none; border-radius: 8px; font-size: 16px; font-weight: bold;">
					Подтвердить email
				</a>
			</p>
		 <p>Или перейдите по ссылке вручную:</p>
			<p style="word-break: break-all; color: #6b7280; font-size: 14px;">%s</p>
			<p style="color: #9ca3af; font-size: 12px; margin-top: 30px;">
				Если вы не регистрировались в %s, проигнорируйте это письмо.
			</p>
		</div>
	`, fromName, verifyURL, verifyURL, fromName)
}

func (m *Mailer) buildResetBody(fromName string, resetURL string) string {
	return fmt.Sprintf(`
		<div style="max-width: 500px; margin: 0 auto; font-family: Arial, sans-serif; padding: 20px;">
			<h2 style="color: #6b21a8;">Сброс пароля</h2>
			<p>Мы получили запрос на сброс пароля. Нажмите кнопку ниже для создания нового пароля:</p>
			<p style="text-align: center; margin: 30px 0;">
				<a href="%s" 
				   style="background-color: #6b21a8; color: white; padding: 14px 32px; 
						  text-decoration: none; border-radius: 8px; font-size: 16px; font-weight: bold;">
					Сбросить пароль
				</a>
			</p>
			<p style="word-break: break-all; color: #6b7280; font-size: 14px;">%s</p>
			<p style="color: #9ca3af; font-size: 12px; margin-top: 30px;">
				Если вы не запрашивали сброс пароля, проигнорируйте это письмо.
			</p>
		</div>
	`, resetURL, resetURL)
}

// isSMTPConfigured возвращает true если SMTP настроен
func (m *Mailer) isSMTPConfigured() bool {
	return m.host != "" && m.user != "" && m.pass != ""
}

// buildVerificationURL собирает полный URL для подтверждения email
func (m *Mailer) buildVerificationURL(token string) string {
	return fmt.Sprintf("%s/auth.html?verify=%s", strings.TrimRight(m.webURL, "/"), token)
}
