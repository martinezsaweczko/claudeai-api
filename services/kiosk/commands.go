package kiosk

import (
	"context"
	"fmt"
	"regexp"

	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
)

// Commands returns the WhatsApp commands of the kiosk (help, lists, download links)
func (s *Service) Commands(botNumber, version string) []whatsapp.Command {
	return []whatsapp.Command{
		{Name: "help_quiosco", Pattern: regexp.MustCompile(`^(?i)quiosco`), Handler: s.helpHandler(version)},
		{Name: "help_ayuda", Pattern: regexp.MustCompile(`^@` + regexp.QuoteMeta(botNumber) + `\s+(?i:ayuda)$`), Handler: s.helpHandler(version)},
		{Name: "list_nacional", Pattern: regexp.MustCompile(`^(?i)nacional\s*`), Handler: s.listHandler("nacional")},
		{Name: "list_internacional", Pattern: regexp.MustCompile(`^(?i)internacional\s*`), Handler: s.listHandler("internacional")},
		{Name: "list_magazine", Pattern: regexp.MustCompile(`^(?i)revista\s*`), Handler: s.listHandler("magazine")},
		{Name: "link_nacional", Pattern: regexp.MustCompile(`^(?i)peri(o|ó)dico:\s*`), Handler: s.linkHandler("nacional")},
		{Name: "link_internacional", Pattern: regexp.MustCompile(`^(?i)newspaper:\s*`), Handler: s.linkHandler("internacional")},
		{Name: "link_magazine", Pattern: regexp.MustCompile(`^(?i)magazine:\s*`), Handler: s.linkHandler("magazine")},
	}
}

// helpHandler replies with the help message
func (s *Service) helpHandler(version string) whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		return sender.ReplyText(ctx, msg, helpText(version))
	}
}

// listHandler replies with the file list of a category
func (s *Service) listHandler(category string) whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		s.log.Info("List requested", "user", msg.PushName, "category", category)
		message, err := s.ListMessage(ctx, category)
		if err != nil {
			return fmt.Errorf("failed to build list: %w", err)
		}
		return sender.ReplyText(ctx, msg, message)
	}
}

// linkHandler replies with a one-time download link for the requested file
func (s *Service) linkHandler(category string) whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		s.log.Info("Download link requested", "user", msg.PushName, "category", category, "body", msg.Body)

		index, err := ParseIndex(msg.Body)
		if err != nil {
			return sender.ReplyText(ctx, msg, "No he podido entender el número de la lista. Ejemplo: *_periodico:2_*")
		}

		link, fileName, err := s.Link(ctx, category, index, msg.PushName)
		if err != nil {
			return sender.ReplyText(ctx, msg, err.Error())
		}

		return sender.ReplyText(ctx, msg, BuildLinkMessage(link, fileName))
	}
}

// helpText builds the help message (ported from the Node version)
func helpText(version string) string {
	return "Hola, soy vuestro quiosquero. Aqui te recuerdo como funciono:\n" +
		"Escribe *nacional*, *internacional* or *revista* para obtener la listas de perdiodicos/revistas a nivel nacional, internacional, y revistas succesivamente\n" +
		"Para pedir un periódico de ámbito nacional, escribe *periodico: _num_lista_*\n" +
		"Para pedir un periódico de ámbito internacional, escribe *newspaper: _num_lista_*\n" +
		"Para pedir un magazine de ámbito internacional, escribe *magazine: _num_lista_*\n" +
		"Ejemplo: _periodico:23_\n" +
		"*Subscripciones*\n" +
		"Para crear una subscripcion: subs: *hola* (Cuando se reciba un fihero que cumpla la condicion te notificará automaticamente)\n" +
		"Para borrar todas tus subscripciones: del_subs\n" +
		"Para listar todas las subscripciones: list_subs\n" +
		"Para transferir un mensaje a audio: audio: *mensaje*\n" +
		"Para crear una imagen con texto: imagen: *texto*\n" +
		"Para obtener el tiempo de una ciudad: tiempo: *ciudad*\n" +
		"Para obtener la previsión de las próximas 24 horas de una ciudad: prevision: *ciudad*\n" +
		"Para obtener el grafico del precio del KWH en la peninsula escribe: electricidad\n" +
		"Para obtener una cita aleatoria: quote\n" +
		"Cómprame un cafe si quieres: https://bmc.link/bonkillaT\n" +
		"Version app: " + version + "\n"
}
