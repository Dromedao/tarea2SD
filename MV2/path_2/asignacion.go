package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"sync"
	"time"

	pb "mv2/proto"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

const maxRetries = 5
const retryDelay = 3 * time.Second

type server struct {
	pb.UnimplementedAssignServiceServer
	mongoClient *mongo.Client
	amqpChannel *amqp.Channel
	mu          sync.Mutex
}

type Drone struct {
	ID        primitive.ObjectID `bson:"_id"`
	DroneID   string             `bson:"id"`
	Latitude  float64            `bson:"latitude"`
	Longitude float64            `bson:"longitude"`
	Status    string             `bson:"status"`
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

func (s *server) AssignDrone(ctx context.Context, req *pb.EmergencyRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("Recibida petición para emergencia: '%s'", req.Name)
	emergenciesCollection := s.mongoClient.Database("emergencias").Collection("emergencias")

	var existingEmergency bson.M
	err := emergenciesCollection.FindOne(context.Background(), bson.M{"name": req.Name}).Decode(&existingEmergency)
	if err == nil {
		log.Printf("INFO: Petición duplicada para '%s'. Estado actual: '%s'. Ignorando.", req.Name, existingEmergency["status"])
		return &emptypb.Empty{}, nil
	}
	if err != mongo.ErrNoDocuments {
		log.Printf("Error de BD al chequear idempotencia: %v", err)
		return nil, err
	}

	log.Printf("Procesando '%s' como una nueva emergencia.", req.Name)

	doc := bson.M{
		"name": req.Name, "latitude": req.Latitude, "longitude": req.Longitude,
		"magnitude": req.Magnitude, "status": "En curso",
	}
	s.publishEmergencyToRegistry(doc)

	drone, err := s.findNearestDrone(req.Latitude, req.Longitude)
	if err != nil || drone == nil {
		log.Printf("❌ No se pudo asignar un dron para la emergencia '%s'.", req.Name)
		return nil, fmt.Errorf("no hay drones disponibles")
	}
	log.Printf("Dron %s asignado a la emergencia.", drone.DroneID)

	err = llamarADron(req, drone.DroneID)
	if err != nil {
		log.Printf("❌ Error en la misión del dron: %v", err)
	} else {
		log.Println("✅ Dron informó que la emergencia fue extinguida. El ciclo de asignación ha terminado.")
	}

	return &emptypb.Empty{}, nil
}

func (s *server) publishEmergencyToRegistry(doc bson.M) {
	body, err := json.Marshal(doc)
	if err != nil {
		log.Printf("❌ Error serializando emergencia para RabbitMQ: %v", err)
		return
	}

	err = withRetry(func() error {
		return s.amqpChannel.PublishWithContext(
			context.Background(), "", "registro_queue", false, false,
			amqp.Publishing{ContentType: "application/json", Body: body},
		)
	}, "Publicar emergencia en RabbitMQ")

	if err != nil {
		log.Printf("❌ Error al publicar emergencia en RabbitMQ: %v", err)
	} else {
		log.Println("✅ Emergencia publicada en RabbitMQ para el servicio de registro.")
	}
}

func (s *server) findNearestDrone(emLat, emLon float64) (*Drone, error) {
	collection := s.mongoClient.Database("emergencias").Collection("drones")
	var drones []Drone
	var nearest *Drone

	err := withRetry(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cursor, findErr := collection.Find(ctx, bson.M{"status": "available"})
		if findErr != nil {
			return findErr
		}
		defer cursor.Close(ctx)
		if err := cursor.All(ctx, &drones); err != nil {
			return err
		}
		if len(drones) == 0 {
			return fmt.Errorf("no hay drones con status 'available'")
		}

		minDist := math.MaxFloat64
		for _, d := range drones {
			dist := math.Sqrt(math.Pow(d.Latitude-emLat, 2) + math.Pow(d.Longitude-emLon, 2))
			if dist < minDist {
				minDist = dist
				currentDrone := d
				nearest = &currentDrone
			}
		}
		if nearest == nil {
			return fmt.Errorf("no se pudo determinar el dron más cercano")
		}

		update := bson.M{"$set": bson.M{"status": "assigned"}}
		_, updateErr := collection.UpdateByID(ctx, nearest.ID, update)
		if updateErr != nil {
			nearest = nil
			return updateErr
		}
		return nil
	}, "Buscar y reservar dron disponible")

	if err != nil {
		return nil, err
	}
	return nearest, nil
}

func llamarADron(emergency *pb.EmergencyRequest, droneID string) error {
	var conn *grpc.ClientConn
	var err error

	err = withRetry(func() error {
		var dialErr error
		conn, dialErr = grpc.Dial("localhost:50052", grpc.WithTransportCredentials(insecure.NewCredentials()))
		return dialErr
	}, "Conexión con servicio de drones")
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pb.NewDroneServiceClient(conn)
	req := &pb.DroneEmergencyRequest{
		Name: emergency.Name, Latitude: emergency.Latitude, Longitude: emergency.Longitude,
		Magnitude: emergency.Magnitude, DroneId: droneID,
	}

	err = withRetry(func() error {
		_, rpcErr := client.AtenderEmergencia(context.Background(), req)
		return rpcErr
	}, "Llamada RPC AtenderEmergencia")
	return err
}

func main() {
	var mongoClient *mongo.Client
	var amqpConn *amqp.Connection
	var ch *amqp.Channel
	var lis net.Listener
	var err error

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

	err = withRetry(func() error {
		var connErr, chErr error
		amqpConn, connErr = amqp.Dial("amqp://guest:guest@localhost:5672/")
		if connErr != nil {
			return connErr
		}
		ch, chErr = amqpConn.Channel()
		if chErr == nil {
			_, chErr = ch.QueueDeclare("registro_queue", false, false, false, false, nil)
		}
		return chErr
	}, "Conexión a RabbitMQ y declaración de cola")
	if err != nil {
		log.Fatalf(err.Error())
	}
	defer amqpConn.Close()
	defer ch.Close()
	log.Println("✅ Conexión exitosa con RabbitMQ.")

	err = withRetry(func() error {
		var listenErr error
		lis, listenErr = net.Listen("tcp", ":50051")
		return listenErr
	}, "Iniciar listener en puerto 50051")
	if err != nil {
		log.Fatalf(err.Error())
	}
	defer lis.Close()

	s := grpc.NewServer()
	pb.RegisterAssignServiceServer(s, &server{mongoClient: mongoClient, amqpChannel: ch})
	log.Println("🚀 Servidor de asignación escuchando en puerto 50051")
	if err := s.Serve(lis); err != nil {
		log.Fatalf("Error al iniciar servidor gRPC: %v", err)
	}
}
