import pika
import pymongo
import json
import time
import logging

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')

MAX_RETRIES = 20
RETRY_DELAY = 10  
MONGO_URI = "mongodb://localhost:27017/"
RABBITMQ_HOST = 'localhost'
QUEUE_NAME = 'registro_queue' 

def connect_to_mongodb():
    """Intenta conectar a MongoDB con reintentos."""
    for i in range(MAX_RETRIES):
        try:
            client = pymongo.MongoClient(MONGO_URI)
            client.admin.command('ping')
            logging.info("✅ Conexión exitosa con MongoDB.")
            return client
        except pymongo.errors.ConnectionFailure as e:
            logging.warning(f"No se pudo conectar a MongoDB (intento {i+1}/{MAX_RETRIES}): {e}")
            time.sleep(RETRY_DELAY)
    logging.error("❌ Fallo definitivo al conectar con MongoDB después de varios intentos.")
    return None

def connect_to_rabbitmq():
    """Intenta conectar a RabbitMQ con reintentos."""
    for i in range(MAX_RETRIES):
        try:
            connection = pika.BlockingConnection(pika.ConnectionParameters(RABBITMQ_HOST))
            logging.info("✅ Conexión exitosa con RabbitMQ.")
            return connection
        except pika.exceptions.AMQPConnectionError as e:
            logging.warning(f"No se pudo conectar a RabbitMQ (intento {i+1}/{MAX_RETRIES}): {e}")
            time.sleep(RETRY_DELAY)
    logging.error("❌ Fallo definitivo al conectar con RabbitMQ después de varios intentos.")
    return None

def main():
    mongo_client = connect_to_mongodb()
    if not mongo_client:
        return 

    db = mongo_client["emergencias"]
    collection = db["emergencias"]

    connection = connect_to_rabbitmq()
    if not connection:
        return
    
    channel = connection.channel()

    channel.queue_declare(queue=QUEUE_NAME)

    def callback(ch, method, properties, body):
        """
        Procesa un mensaje de RabbitMQ: lo decodifica de JSON y decide si
        insertar un nuevo documento o actualizar uno existente en MongoDB.
        """
        try:
            message_str = body.decode('utf-8')
            data = json.loads(message_str)
            logging.info(f"📦 Mensaje JSON recibido: {data}")

            if "status" in data and data["status"] == "Extinguido":
                emergency_name = data.get("name")
                if emergency_name:
                    logging.info(f"Actualizando estado de '{emergency_name}' a 'Extinguido' en MongoDB.")
                    collection.update_one(
                        {"name": emergency_name, "status": "En curso"},
                        {"$set": {"status": "Extinguido"}}
                    )
                else:
                    logging.warning("Mensaje de actualización de estado no contiene 'name'")
            else:
                
                logging.info(f"Insertando nueva emergencia '{data.get('name')}' en MongoDB.")
                collection.insert_one(data)

        except json.JSONDecodeError:
            logging.error(f"Error decodificando JSON del mensaje: {body.decode()}")
        except Exception as e:
            logging.error(f"Error procesando el mensaje: {e}")

    channel.basic_consume(queue=QUEUE_NAME, on_message_callback=callback, auto_ack=True)

    logging.info(f"🟢 Esperando mensajes en la cola '{QUEUE_NAME}'. Presiona CTRL+C para salir.")
    try:
        channel.start_consuming()
    except KeyboardInterrupt:
        logging.info("Cerrando conexiones...")
        connection.close()
        mongo_client.close()
        logging.info("Conexiones cerradas. Adiós.")

if __name__ == '__main__':
    main()