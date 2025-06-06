package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"time"

	pb "mv3/proto"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"
)

const maxRetries = 20
const retryDelay = 10 * time.Second

type server struct {
	pb.UnimplementedDroneServiceServer
	mongoClient *mongo.Client
	amqpChannel *amqp.Channel
}

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

func (s *server) obtenerPosicionDron(droneID string) (float64, float64, error) {
	coll := s.mongoClient.Database("emergencias").Collection("drones")
	var dron struct {
		Latitude  float64 `bson:"latitude"`
		Longitude float64 `bson:"longitude"`
	}
	err := withRetry(func() error {
		return coll.FindOne(context.Background(), bson.M{"id": droneID}).Decode(&dron)
	}, fmt.Sprintf("Obtener posición del dron %s", droneID))

	if err != nil {
		return 0, 0, err
	}
	return dron.Latitude, dron.Longitude, nil
}

func (s *server) AtenderEmergencia(ctx context.Context, req *pb.DroneEmergencyRequest) (*pb.DroneEmergencyResponse, error) {
	log.Printf("🚁 Emergencia recibida: %s, magnitud: %d con dron %s", req.Name, req.Magnitude, req.DroneId)

	lat, lon, err := s.obtenerPosicionDron(req.DroneId)
	if err != nil {
		log.Printf("Error obteniendo posición del dron: %v", err)
		lat, lon = 0, 0
	}

	dist := math.Sqrt(math.Pow(req.Latitude-lat, 2) + math.Pow(req.Longitude-lon, 2))
	tiempoDesplazamiento := time.Duration(0.5*dist*1000) * time.Millisecond
	tiempoApagado := time.Duration(2*req.Magnitude*1000) * time.Millisecond

	log.Printf("Moviéndose hacia la emergencia, tomará: %.2f segundos", tiempoDesplazamiento.Seconds())
	log.Printf("Apagando el incendio, tomará: %.2f segundos", tiempoApagado.Seconds())

	go s.publicarActualizacionesParaMonitor(req, tiempoDesplazamiento, tiempoApagado)

	time.Sleep(tiempoDesplazamiento + tiempoApagado)

	log.Printf("✅ Emergencia '%s' extinguida.", req.Name)

	s.actualizarPosicionDron(req.DroneId, req.Latitude, req.Longitude)
	s.publicarEstadoExtinguidoParaRegistro(req)

	return &pb.DroneEmergencyResponse{
		Status: "Extinguido",
	}, nil
}

func (s *server) publicarActualizacionesParaMonitor(req *pb.DroneEmergencyRequest, tDesplazamiento, tApagado time.Duration) {

	publishMessage := func(status string) {
		updateMsg := map[string]string{
			"status":         status,
			"drone_id":       req.DroneId,
			"emergency_name": req.Name,
		}
		body, _ := json.Marshal(updateMsg)
		s.amqpChannel.Publish("", "monitoreo_queue", false, false, amqp.Publishing{ContentType: "application/json", Body: body})
	}

	desplazamientoTicker := time.NewTicker(5 * time.Second)
	finDesplazamiento := time.After(tDesplazamiento)
	for {
		select {
		case <-desplazamientoTicker.C:
			publishMessage("desplazamiento")
		case <-finDesplazamiento:
			desplazamientoTicker.Stop()
			goto fase_apagado
		}
	}

fase_apagado:
	apagadoTicker := time.NewTicker(5 * time.Second)
	finApagado := time.After(tApagado)
	for {
		select {
		case <-apagadoTicker.C:
			publishMessage("apagado")
		case <-finApagado:
			apagadoTicker.Stop()
			goto fase_final
		}
	}

fase_final:
	publishMessage("extinguido")
	log.Println("Fin del ciclo de publicación de actualizaciones para el monitor.")
}

func (s *server) publicarEstadoExtinguidoParaRegistro(req *pb.DroneEmergencyRequest) {
	updateMsg := map[string]string{
		"name":   req.Name,
		"status": "Extinguido",
	}
	body, err := json.Marshal(updateMsg)
	if err != nil {
		log.Printf("Error serializando mensaje para registro: %v", err)
		return
	}

	err = s.amqpChannel.Publish("", "registro_queue", false, false, amqp.Publishing{ContentType: "application/json", Body: body})

	if err != nil {
		log.Printf("❌ Error publicando estado extinguido a registro: %v", err)
	} else {
		log.Printf("✅ Publicado estado 'Extinguido' de '%s' para el servicio de registro.", req.Name)
	}
}

func (s *server) actualizarPosicionDron(droneID string, lat, lon float64) {
	coll := s.mongoClient.Database("emergencias").Collection("drones")

	filter := bson.M{"id": droneID}
	update := bson.M{
		"$set": bson.M{
			"latitude":  lat,
			"longitude": lon,
			"status":    "available",
		},
	}
	err := withRetry(func() error {
		_, updateErr := coll.UpdateOne(context.Background(), filter, update)
		return updateErr
	}, fmt.Sprintf("Actualizar posición y estado del dron %s", droneID))

	if err != nil {
		log.Printf("❌ Error actualizando dron en MongoDB: %v", err)
	} else {
		log.Printf("✅ Posición y estado de dron %s actualizados en MongoDB.", droneID)
	}
}

func main() {
	log.Println("🚀 Iniciando servicio de drones...")
	var err error

	var mongoClient *mongo.Client
	err = withRetry(func() error {
		var connErr error
		mongoClient, connErr = mongo.Connect(context.Background(), options.Client().ApplyURI("mongodb://localhost:27017"))
		return connErr
	}, "Conexión a MongoDB")
	if err != nil {
		log.Fatalf(err.Error())
	}
	defer mongoClient.Disconnect(context.Background())
	log.Println("✅ Conexión exitosa con MongoDB.")

	var amqpConn *amqp.Connection
	var ch *amqp.Channel
	err = withRetry(func() error {
		var connErr, chErr error
		amqpConn, connErr = amqp.Dial("amqp://guest:guest@localhost:5672/")
		if connErr != nil {
			return connErr
		}
		ch, chErr = amqpConn.Channel()
		if chErr == nil {
			_, chErr = ch.QueueDeclare("monitoreo_queue", false, false, false, false, nil)
			if chErr != nil {
				return fmt.Errorf("fallo al declarar cola de monitoreo: %w", chErr)
			}

			_, chErr = ch.QueueDeclare("registro_queue", false, false, false, false, nil)
			if chErr != nil {
				return fmt.Errorf("fallo al declarar cola de registro: %w", chErr)
			}
		}
		return chErr
	}, "Conexión a RabbitMQ y declaración de colas")
	if err != nil {
		log.Fatalf(err.Error())
	}
	defer amqpConn.Close()
	defer ch.Close()
	log.Println("✅ Conexión exitosa con RabbitMQ.")

	var lis net.Listener
	err = withRetry(func() error {
		var listenErr error
		lis, listenErr = net.Listen("tcp", ":50052")
		return listenErr
	}, "Iniciar listener en puerto 50052")
	if err != nil {
		log.Fatalf(err.Error())
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	pb.RegisterDroneServiceServer(grpcServer, &server{
		mongoClient: mongoClient,
		amqpChannel: ch,
	})

	log.Println("🟢 Servidor de drones escuchando en puerto 50052")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Error al iniciar servidor gRPC: %v", err)
	}
}
