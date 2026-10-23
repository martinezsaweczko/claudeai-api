package subscription

import (
	"context"
	"regexp"

	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
)

// subsPrefix strips "subs:" or "subscripción:" (any case) from the message body
var subsPrefix = regexp.MustCompile(`^(?i)s(ubs:|ubscripci(o|ó)n:)`)

// Commands returns the WhatsApp commands for subscription management
func (s *Service) Commands() []whatsapp.Command {
	return []whatsapp.Command{
		{Name: "subs_create", Pattern: regexp.MustCompile(`^(?i)s(ubs:|ubscripci(o|ó)n:)\s*`), Handler: s.subscribeHandler()},
		{Name: "subs_delete", Pattern: regexp.MustCompile(`^(?i)d(el|elete)_s(ubs|ubscripci(o|ó)n).*`), Handler: s.deleteHandler()},
		{Name: "subs_list", Pattern: regexp.MustCompile(`^(?i)list_s(ubs|ubscripci(o|ó)n).*`), Handler: s.listHandler()},
	}
}

func (s *Service) subscribeHandler() whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		text := subsPrefix.ReplaceAllString(msg.Body, "")
		if err := s.Subscribe(ctx, msg, text); err != nil {
			return sender.ReplyText(ctx, msg, "No he podido crear la subscripción: "+err.Error())
		}
		return sender.ReplyText(ctx, msg, "Te has subscrito correctamente a "+text)
	}
}

func (s *Service) deleteHandler() whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		if err := s.Delete(ctx, msg); err != nil {
			return sender.ReplyText(ctx, msg, "No he podido borrar tus subscripciones")
		}
		return sender.ReplyText(ctx, msg, "Tus subscripciones se han borrado correctamente")
	}
}

func (s *Service) listHandler() whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		list, err := s.List(ctx, msg)
		if err != nil {
			return sender.ReplyText(ctx, msg, "No he podido obtener tus subscripciones")
		}
		return sender.ReplyText(ctx, msg, list)
	}
}
