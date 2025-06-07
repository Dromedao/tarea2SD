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

func (s *server) obtenerPosicionDron(droneID string) (float64, float64, error) {
	// obtenerPosicionDron recupera la latitud y longitud de un dron específico desde la base de datos MongoDB.
	//
	// La función consulta la colección "drones" en la base de datos "emergencias"
	// para encontrar el dron con el 'droneID' proporcionado. La operación de búsqueda
	// incluye reintentos para manejar fallos temporales de conexión a la base de datos.
	//
	// Parámetros:
	//   droneID: Una cadena que representa el identificador único del dron cuya posición se desea obtener.
	//
	// Retorna:
	//   float64: La latitud del dron.
	//   float64: La longitud del dron.
	//   error: nil si la operación es exitosa y se encuentra la posición del dron,
	//          o un error si el dron no se encuentra o si ocurre un fallo en la base de datos después de los reintentos.
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
	// AtenderEmergencia es un método RPC que simula la atención de una emergencia por un dron.
	//
	// Esta función calcula el tiempo de desplazamiento y el tiempo necesario para apagar la emergencia
	// basándose en la distancia del dron a la emergencia y la magnitud de esta.
	// Publica actualizaciones de estado a un servicio de monitoreo en segundo plano
	// y luego simula la duración de la operación esperando los tiempos calculados.
	// Finalmente, actualiza la posición del dron y notifica al servicio de registro
	// que la emergencia ha sido extinguida.
	//
	// Parámetros:
	//   ctx: El contexto de la solicitud gRPC.
	//   req: Un puntero a un objeto *pb.DroneEmergencyRequest que contiene
	//        los detalles de la emergencia (nombre, coordenadas, magnitud) y el ID del dron asignado.
	//
	// Retorna:
	//   *pb.DroneEmergencyResponse: Un objeto de respuesta que indica el estado final de la emergencia ("Extinguido").
	//   error: nil si la operación se completa sin errores.
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
	// publicarActualizacionesParaMonitor simula y publica actualizaciones de estado de un dron
	// a un servicio de monitoreo a través de RabbitMQ, cubriendo las fases de desplazamiento y apagado.
	//
	// Esta función se ejecuta en una goroutine separada para no bloquear la ejecución principal.
	// Utiliza "tickers" para enviar mensajes de estado periódicamente (cada 5 segundos)
	// durante las fases de desplazamiento y apagado del dron. Una vez que una fase termina
	// (determinada por 'tDesplazamiento' y 'tApagado'), pasa a la siguiente fase y,
	// finalmente, publica un mensaje de "extinguido".
	//
	// Parámetros:
	//   req: Un puntero a un objeto *pb.DroneEmergencyRequest que contiene
	//        los detalles de la emergencia (ID del dron, nombre de la emergencia) necesarios para las actualizaciones.
	//   tDesplazamiento: La duración total simulada que el dron tarda en desplazarse a la emergencia.
	//   tApagado: La duración total simulada que el dron tarda en apagar la emergencia.
	//
	// Retorna:
	//   Ninguno.
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
	// actualizarPosicionDron actualiza la latitud, longitud y el estado de un dron en la base de datos MongoDB.
	//
	// Esta función toma el ID de un dron junto con sus nuevas coordenadas.
	// Establece el estado del dron a "available" (disponible) y actualiza estas propiedades
	// en la colección "drones" de la base de datos "emergencias". La operación de actualización
	// incluye reintentos para manejar posibles fallos temporales de conexión a la base de datos.
	// Registra el éxito o el fallo de la operación.
	//
	// Parámetros:
	//   droneID: Una cadena que identifica el dron cuya posición y estado se van a actualizar.
	//   lat: La nueva latitud del dron.
	//   lon: La nueva longitud del dron.
	//
	// Retorna:
	//   Ninguno.
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
