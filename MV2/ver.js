// Seleccionar la base de datos
use emergencias

// Listar todas las colecciones (tablas) en la base de datos
print("Colecciones en la base de datos 'emergencias':")
printjson(db.getCollectionNames())

// Para cada colección, mostrar todos sus documentos
db.getCollectionNames().forEach(function(collName) {
    print("\nDocumentos en la colección: " + collName)
    var docs = db.getCollection(collName).find().toArray()
    printjson(docs)
})
