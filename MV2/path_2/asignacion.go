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
	// withRetry ejecuta una función 'action' con reintentos en caso de fallo.
	//
	// Esta función intenta ejecutar la 'action' un número máximo de veces (definido por 'maxRetries').
	// Si la 'action' retorna un error, espera un 'retryDelay' antes de reintentar.
	// Muestra un mensaje de log en cada fallo y un mensaje final si todos los intentos fallan.
	//
	// Parámetros:
	//   action: Una función sin parámetros que retorna un error. Esta es la operación que se intentará.
	//   description: Una cadena que describe la acción que se está realizando, utilizada para los mensajes de log.
	//
	// Retorna:
	//   error: nil si la 'action' se ejecuta exitosamente en cualquier intento,
	//          o un error que describe el fallo definitivo si todos los reintentos fallan.
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
	// AssignDrone es un método RPC que recibe una solicitud de emergencia,
	// verifica si es una emergencia nueva o duplicada, y si es nueva, la procesa.
	//
	// El proceso incluye:
	// 1. Bloquear para garantizar la exclusividad en el procesamiento de emergencias.
	// 2. Comprobar en la base de datos si la emergencia ya existe para manejar la idempotencia.
	// 3. Si es una nueva emergencia, la publica en el servicio de registro (RabbitMQ).
	// 4. Busca el dron disponible más cercano y lo asigna a la emergencia.
	// 5. Llama al servicio del dron para que atienda la emergencia.
	// 6. Registra el resultado de la asignación y la misión del dron.
	//
	// Parámetros:
	//   ctx: El contexto de la solicitud gRPC, que puede llevar información como cancelaciones o plazos.
	//   req: Un puntero a un objeto *pb.EmergencyRequest que contiene los detalles de la emergencia
	//        (nombre, latitud, longitud, magnitud).
	//
	// Retorna:
	//   *emptypb.Empty: Un mensaje vacío que indica que la operación fue procesada (exitosamente o con un error manejado).
	//   error: nil si la operación se completó sin errores fatales, o un error si ocurre un problema
	//          crítico como un fallo de base de datos no relacionado con la ausencia de documentos,
	//          o si no se puede asignar un dron.
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
	// publishEmergencyToRegistry serializa una emergencia en formato JSON y la publica
	// en una cola de RabbitMQ para que sea procesada por el servicio de registro.
	//
	// La función primero convierte el documento BSON de la emergencia a JSON.
	// Luego, intenta publicar este mensaje JSON en la cola "registro_queue" de RabbitMQ.
	// La operación de publicación incluye reintentos para manejar fallos temporales.
	// Imprime mensajes de log indicando el éxito o el fallo de la publicación.
	//
	// Parámetros:
	//   doc: Un documento BSON (bson.M) que representa la emergencia a publicar.
	//
	// Retorna:
	//   Ninguno.
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
	// findNearestDrone busca el dron más cercano disponible a una ubicación de emergencia
	// y lo marca como "assigned" (asignado) en la base de datos.
	//
	// La función consulta la colección "drones" en MongoDB para encontrar todos los drones con estado "available".
	// Luego, calcula la distancia euclidiana de cada dron a las coordenadas de la emergencia
	// y selecciona el dron más cercano. Finalmente, actualiza el estado de este dron a "assigned"
	// en la base de datos. Toda la operación de búsqueda y actualización se realiza con reintentos
	// utilizando la función `withRetry`.
	//
	// Parámetros:
	//   emLat: La latitud de la emergencia.
	//   emLon: La longitud de la emergencia.
	//
	// Retorna:
	//   *Drone: Un puntero a la estructura Drone del dron más cercano y asignado,
	//           o nil si no se encuentra un dron disponible o si ocurre un error.
	//   error: nil si la operación es exitosa, o un error si falla la conexión a la base de datos,
	//          no hay drones disponibles, o si la actualización del estado del dron falla.
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
	// llamarADron intenta contactar a un servicio de drones para atender una emergencia específica.
	//
	// Esta función primero intenta establecer una conexión gRPC con el servicio de drones,
	// reintentando la conexión en caso de fallo. Una vez conectado, construye una solicitud
	// de emergencia para el dron especificado y llama al método RPC 'AtenderEmergencia'
	// del servicio de drones, también con reintentos.
	//
	// Parámetros:
	//   emergency: Un puntero a un objeto *pb.EmergencyRequest que contiene los detalles de la emergencia.
	//   droneID: Una cadena que identifica al dron específico al que se le asignará la emergencia.
	//
	// Retorna:
	//   error: nil si la conexión y la llamada RPC son exitosas, o un error si alguna de las
	//          operaciones (conexión o llamada RPC) falla definitivamente después de los reintentos.
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
