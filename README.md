# Integrantes
- Benjamin Paulsen - 202173017-6  
- Matías Guerra - 202173137-7  

# Aclaración
A lo largo del README se habla de maquina virtual 1,2 y 3. Por cada maquina se entiede:
- MV1: IP 10.10.28.14
- MV2: IP 10.10.28.15
- MV3: IP 10.10.28.16

## Aclaración importante
Si bien logramos realizar la mayor parte de la configuración, como el cambio de ip a las de cada maquina, creación de usuario para que rabbitMQ permita conexiónes externas, configuracion de mongo para conexiones externas, entre otras, tuvimos un problema con la maquina virtual 2 lo cual no nos permitió probar el código ya que no tenemos acceso a dicha maquina.

Por lo mismo, el código en github hace referencia a localhost, ya que es donde lo creamos pero en las maquinas como tal están puestas las ip y demases para que hagan la comunicación a la maquina correspondiente, pero debido al inconveniente no pudimos probar el código.

# Arquitectura del Sistema

- **Máquina Virtual 1 (MV1):** Interfaz de Usuario
Cliente (cliente.go): Punto de entrada del sistema. Carga las emergencias desde un archivo JSON, las envía al Servicio de Asignación y se suscribe al Servicio de Monitoreo para mostrar actualizaciones en la consola.
Servicio de Monitoreo (monitoreo.go): Actúa como un puente. Recibe actualizaciones de estado de los drones vía RabbitMQ y las retransmite a los clientes conectados a través de un stream gRPC.
- **Máquina Virtual 2 (MV2):**
**Servicio de Asignación (asignacion.go):**  Recibe las emergencias, consulta la base de datos para encontrar el dron disponible más cercano, y delega la misión al Servicio de Drones.
**Servicio de Registro (registro.py):** Único servicio encargado de la persistencia de las emergencias. Escucha eventos en RabbitMQ para crear y actualizar los registros de emergencias en MongoDB.
**Base de Datos (MongoDB):** Almacena el estado y posición de los drones, y el historial completo de las emergencias.
**Broker de Mensajes (RabbitMQ):** Gestiona las colas de comunicación asíncrona entre los servicios.
- **Máquina Virtual 3 (MV3):** **Operaciones
Servicio de Drones (drones.go):** Simula las operaciones físicas de los drones. Recibe órdenes del Servicio de Asignación, simula los tiempos de vuelo y extinción, y reporta su progreso al Servicio de Monitoreo y el resultado final al Servicio de Registro.

# Instrucciones de Despliegue y Ejecución
Siga estos pasos para configurar y ejecutar el sistema completo en el entorno de máquinas virtuales.

**Paso 1:** Configuración de Red y Firewall
En cada máquina virtual, asegúrese de que los puertos necesarios estén abiertos.

En MV2: Abra los puertos para MongoDB y RabbitMQ.
```Bash

sudo ufw allow 27017/tcp # Puerto para MongoDB
sudo ufw allow 5672/tcp # Puerto para RabbitMQ
En los servidores gRPC: Abra los puertos correspondientes en cada VM.
```
En MV2: 
```
sudo ufw allow 50051/tcp (para Asignación)
```
En MV3: 
```
sudo ufw allow 50052/tcp (para Drones)
```
En MV1: 
```
sudo ufw allow 50053/tcp (para Monitoreo)
```

**Paso 2:** Configuración de Servicios en MV2
MongoDB: Modifique el archivo de configuración para permitir conexiones externas.

Edite el archivo: 
```bash
sudo nano /etc/mongod.conf
```
Busque la sección net y cambie bindIp de 127.0.0.1 a 0.0.0.0.
Guarde y reinicie el servicio: sudo systemctl restart mongodb (o mongod si ese es el nombre del servicio en su sistema).
RabbitMQ: Cree un usuario con permisos para conexiones remotas.

```Bash
sudo rabbitmqctl add_user <usuario> <contraseña>
sudo rabbitmqctl set_user_tags <usuario> administrator
sudo rabbitmqctl set_permissions -p / <usuario> ".*" ".*" ".*"
```

**Paso 3:** 

Configuración del Código Fuente
Antes de ejecutar, debe actualizar las cadenas de conexión en todos los archivos de código (.go y .py) para que dejen de apuntar a localhost y apunten a la dirección IP de la MV2.

Ejemplo en Go: 
```
mongo.Connect(..., options.Client().ApplyURI("mongodb://<IP_DE_LA_MV2>:27017"))
```
Ejemplo en Go: 
```
amqp.Dial("amqp://<usuario>:<contraseña>@<IP_DE_LA_MV2>:5672/")
```
Ejemplo en Python: 
```
MONGO_URI = "mongodb://<IP_DE_LA_MV2>:27017/"
```
**Paso 4:** Inicialización de la Base de Datos
En la MV2, ejecute el script init.js para limpiar y poblar la base de datos con los drones iniciales.

```Bash
mongosh < init.js
```
**Paso 5:** Ejecución del Sistema
Abra una terminal en cada máquina virtual y ejecute los servicios en el siguiente orden.

- En MV1 (Terminal 1), ejecute el Cliente:

```Bash
./cliente.go emergencia.json
```
Verá en esta terminal la narrativa completa de la gestión de emergencias.

En MV2 (Terminal 2), inicie el Servicio de Asignación:

```Bash
go run path_2/asignacion.go
```

- En MV2 (Terminal 3), inicie el Servicio de Registro:

```Bash
python3 registro.py
```

- En MV1 (Terminal 4), inicie el Servicio de Monitoreo:

```Bash
go run monitoreo.go
```
- En MV3 (Terminal 5), inicie el Servicio de Drones:

```Bash
go run drones.go
```

## Consideraciones Especiales

En el código se trató de lograr lo siguiente
- **Tolerancia a Fallos:** El sistema está diseñado para ser resiliente. En caso de perdidas de conexión o que un servicio esté apagado, los sistemas reintentan conectarse.
- **Idempotencia:** El Servicio de Asignación es idempotente, lo que significa que es capaz de manejar peticiones duplicadas (causadas por timeouts y reintentos del cliente) sin procesar la misma emergencia dos veces.

## 📦 Requisitos Generales

Para compilar y ejecutar este proyecto en el entorno de máquinas virtuales, es necesario tener instalado el siguiente software:

## Lenguajes y Runtimes
- **Go** (versión 1.23.1 o superior)
- **Python** (versión 3.8 o superior)

## Bases de Datos y Brokers de Mensajería
- **MongoDB Server**
- **RabbitMQ Server**

## Librerías de Python
Se pueden instalar usando `pip`:

```bash
pip install pika pymongo
```
## Herramientas de Desarrollo (Go)

### Compilador de Protocol Buffers: protoc

#### Plugins de Go para protoc:

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.28
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.2
```
