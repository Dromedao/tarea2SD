// Selecciona la base de datos
use emergencias

// Limpia la colección de drones si existe
db.drones.deleteMany({})

// Inserta datos iniciales
// Inserta datos iniciales con coordenadas dentro de la grilla [-50, 50]
db.drones.insertMany([
    {
        id: "dron01",
        latitude: -45,   // Coordenada dentro de la grilla
        longitude: -45,  // Coordenada dentro de la grilla
        status: "available",
        last_update: new Date()
    },
    {
        id: "dron02",
        latitude: 0,     // Coordenada dentro de la grilla
        longitude: 0,    // Coordenada dentro de la grilla
        status: "available",
        last_update: new Date()
    },
    {
        id: "dron03",
        latitude: 45,    // Coordenada dentro de la grilla
        longitude: 45,   // Coordenada dentro de la grilla
        status: "available",
        last_update: new Date()
    }
])

// Crea la colección de emergencias si no existe (MongoDB la crea automáticamente al insertar)
db.emergencias.deleteMany({}) // Limpia también emergencias

// Crear índices para mejor rendimiento
db.drones.createIndex({ status: 1 })
db.drones.createIndex({ latitude: 1, longitude: 1 })
db.emergencias.createIndex({ status: 1 })
