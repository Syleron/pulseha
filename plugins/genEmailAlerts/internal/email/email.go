package email

import (
	"net/smtp"
)

func SendEmail(
	username string,
	password string,
	email string,
	host string,
	port string,
	message string,
) error {
	auth := smtp.PlainAuth("", username, password, host)

	err := smtp.SendMail(
		host+":"+port,
		auth,
		username,
		[]string{email},
		[]byte(message),
	)
	if err != nil {
		return err
	}
	return nil
}
