package main

import (
	"fmt"
	"log"
	"net"
	"time"

	pb "mv1/proto"

	amqp "github.com/rabbitmq/amqp091-go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

const maxRetries = 20
const retryDelay = 10 * time.Second

func withRetry(action func() error, description string) error {
	var err error
	for i := 0; i < maxRetries; i++ {
		err = action()
		if err == nil {
			return nil
		}
		log.Printf("Fallo en '%s' (intento %d/%d): %v", description, i+1, maxRetries, err)
		if i < maxRetries-1 {
			time.Sleep(retryDelay)
		}
	}
	return fmt.Errorf("fallo definitivo en '%s' después de %d intentos: %w", description, maxRetries, err)
}

type monitoringServer struct {
	pb.UnimplementedMonitoringServiceServer
	updates chan []byte
}

func (s *monitoringServer) StreamUpdates(req *emptypb.Empty, stream pb.MonitoringService_StreamUpdatesServer) error {
	for {
		select {
		case <-stream.Context().Done():
			log.Println("Cliente desconectado del stream.")
			return nil
		case msg := <-s.updates:
			if err := stream.Send(&pb.UpdateResponse{Message: string(msg)}); err != nil {
				log.Printf("Error enviando actualización al cliente: %v", err)
				return err
			}
		}
	}
}

func main() {
	log.Println("🔍 Iniciando servicio de monitoreo...")

	var amqpConn *amqp.Connection
	var ch *amqp.Channel
	var err error

	err = withRetry(func() error {
		var connErr, chErr error
		amqpConn, connErr = amqp.Dial("amqp://guest:guest@localhost:5672/")
		if connErr != nil {
			return connErr
		}
		ch, chErr = amqpConn.Channel()
		if chErr != nil {
			return chErr
		}
		_, chErr = ch.QueueDeclare(
			"monitoreo_queue",
			false,
			false,
			false,
			false,
			nil,
		)
		return chErr
	}, "Conexión a RabbitMQ y declaración de cola")

	if err != nil {
		log.Fatalf(err.Error())
	}
	defer amqpConn.Close()
	defer ch.Close()
	log.Println("✅ Conexión y declaración de cola exitosa en RabbitMQ.")

	msgs, err := ch.Consume(
		"monitoreo_queue",
		"",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		log.Fatalf("Error al consumir de RabbitMQ: %v", err)
	}

	lis, err := net.Listen("tcp", ":50053")
	if err != nil {
		log.Fatalf("No se pudo escuchar en el puerto 50053: %v", err)
	}

	grpcServer := grpc.NewServer()
	s := &monitoringServer{
		updates: make(chan []byte),
	}
	pb.RegisterMonitoringServiceServer(grpcServer, s)

	go func() {
		log.Println("📞 Servidor gRPC para cliente escuchando en puerto 50053")
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("Error al iniciar servidor gRPC: %v", err)
		}
	}()

	log.Println("🟢 Esperando mensajes de monitoreo...")

	for d := range msgs {
		log.Printf("📡 Mensaje recibido de RabbitMQ: %s", d.Body)
		s.updates <- d.Body
	}
}
