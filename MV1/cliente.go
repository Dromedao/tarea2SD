package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"time"

	pb "mv1/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

type Emergency struct {
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Magnitude int32   `json:"magnitude"`
}

type StatusUpdate struct {
	DroneId       string `json:"drone_id"`
	Status        string `json:"status"`
	EmergencyName string `json:"emergency_name"`
}

func main() {
	log.SetFlags(0)

	emergencias := readEmergenciesFromFile()
	fmt.Println("Emergencias recibidas")

	assignConn := mustConnect("Asignación", "localhost:50051")
	defer assignConn.Close()
	monitoringConn := mustConnect("Monitoreo", "localhost:50053")
	defer monitoringConn.Close()

	assignClient := pb.NewAssignServiceClient(assignConn)

	emergencyDone := make(chan bool)
	announced := make(map[string]bool)

	go listenForUpdates(monitoringConn, emergencyDone, announced)

	for _, em := range emergencias {
		fmt.Printf("Emergencia actual : %s magnitud %d en x=%.0f , y=%.0f\n", em.Name, em.Magnitude, em.Latitude, em.Longitude)

		req := &pb.EmergencyRequest{
			Name:      em.Name,
			Latitude:  em.Latitude,
			Longitude: em.Longitude,
			Magnitude: em.Magnitude,
		}

		go func(request *pb.EmergencyRequest) {
			for i := 0; i < 10; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, err := assignClient.AssignDrone(ctx, request)
				cancel()
				if err == nil {
					return
				}
				time.Sleep(5 * time.Second)
			}
			log.Printf("FALLO DEFINITIVO: No se pudo enviar la emergencia '%s'", request.Name)
		}(req)

		<-emergencyDone
	}
}

func listenForUpdates(conn *grpc.ClientConn, done chan<- bool, announced map[string]bool) {
	// listenForUpdates establece un stream gRPC con el servicio de monitoreo
	// para recibir actualizaciones sobre el estado de las emergencias y drones.
	//
	// La función intenta abrir el stream hasta 20 veces, esperando 5 segundos entre cada intento.
	// Si no logra abrir el stream después de los intentos, cierra el canal 'done'
	// e imprime un mensaje de error, indicando que no se recibirán más actualizaciones.
	//
	// Una vez que el stream se establece, la función entra en un bucle infinito para recibir actualizaciones.
	// Cada actualización es un mensaje JSON que se deserializa a una estructura StatusUpdate.
	// Dependiendo del estado de la emergencia, se imprimen mensajes en la consola.
	// Si una emergencia es "extinguida", envía una señal a través del canal 'done'.
	//
	// Parámetros:
	//   conn: Una conexión gRPC establecida con el servidor de monitoreo.
	//   done: Un canal de solo escritura (chan<- bool) utilizado para enviar una señal
	//         cuando una emergencia ha sido extinguida o si el stream falla definitivamente.
	//   announced: Un mapa (map[string]bool) para llevar un registro de las emergencias
	//              que ya han sido anunciadas para evitar duplicados.
	//
	// Retorna:
	//   Ninguno (la función se ejecuta en un bucle infinito hasta que el stream se desconecta
	//   o falla, momento en el que retorna o cierra el canal 'done').
	monitoringClient := pb.NewMonitoringServiceClient(conn)

	var stream pb.MonitoringService_StreamUpdatesClient
	var err error

	for i := 0; i < 10; i++ {
		stream, err = monitoringClient.StreamUpdates(context.Background(), &emptypb.Empty{})
		if err == nil {
			break
		}
		log.Printf("No se pudo abrir stream con el monitor (intento %d/10): %v", i+1, err)
		time.Sleep(5 * time.Second)
	}

	if err != nil {
		log.Printf("Error definitivo al abrir stream de actualizaciones. No se recibirán más actualizaciones.")
		close(done)
		return
	}

	for {
		update, err := stream.Recv()
		if err != nil {
			log.Printf("Stream de monitoreo desconectado: %v", err)
			return
		}

		var statusUpdate StatusUpdate
		if json.Unmarshal([]byte(update.Message), &statusUpdate) != nil {
			continue
		}

		if !announced[statusUpdate.EmergencyName] && statusUpdate.Status != "extinguido" {
			fmt.Printf("Se ha asignado %s a la emergencia\n", statusUpdate.DroneId)
			announced[statusUpdate.EmergencyName] = true
		}

		switch statusUpdate.Status {
		case "desplazamiento":
			fmt.Println("Dron en camino a emergencia...")
		case "apagado":
			fmt.Println("Dron apagando emergencia...")
		case "extinguido":
			fmt.Printf("%s ha sido extinguido por %s\n", statusUpdate.EmergencyName, statusUpdate.DroneId)
			done <- true
		}
	}
}

func readEmergenciesFromFile() []Emergency {
	// readEmergenciesFromFile lee un archivo JSON de emergencias y lo deserializa en una slice de Emergency.
	//
	// Esta función intenta abrir el archivo especificado como el primer argumento de línea de comandos.
	// Si no se proporciona ningún argumento, por defecto usa "emergencias.json".
	// Lee todo el contenido del archivo y lo parsea como un JSON.
	//
	// Retorna:
	//   []Emergency: Una slice de estructuras Emergency que contiene los datos leídos del archivo.
	//
	// En caso de que ocurra un error al abrir el archivo, la función termina la ejecución del programa con un mensaje fatal.
	args := os.Args
	if len(args) < 2 {
		args = append(args, "emergencias.json")
	}
	file, err := os.Open(args[1])
	if err != nil {
		log.Fatalf("Error abriendo archivo: %v", err)
	}
	defer file.Close()
	byteValue, _ := ioutil.ReadAll(file)
	var emergencies []Emergency
	json.Unmarshal(byteValue, &emergencies)
	return emergencies
}

func mustConnect(serviceName, addr string) *grpc.ClientConn {
	// mustConnect intenta establecer una conexión gRPC con un servicio dado.
	// Reintentará la conexión hasta 20 veces con un intervalo de 10 segundos entre cada intento.
	//
	// Parámetros:
	//   serviceName: Una cadena que representa el nombre del servicio al que se intenta conectar (usado para mensajes de error).
	//   addr: La dirección del servidor gRPC al que conectar (ej. "localhost:50051").
	//
	// Retorna:
	//   *grpc.ClientConn: Un objeto de conexión gRPC si la conexión es exitosa.
	//
	// Si todos los intentos fallan, la función termina la ejecución del programa con un mensaje de error fatal.
	var conn *grpc.ClientConn
	var err error
	for i := 0; i < 20; i++ {
		conn, err = grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			return conn
		}
		time.Sleep(10 * time.Second)
	}
	log.Fatalf("Fallo definitivo al conectar con Serv. de %s: %v", serviceName, err)
	return nil
}
