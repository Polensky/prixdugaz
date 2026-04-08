# Essence

Carte de chaleur des prix d'essence au Québec affichant les prix en temps réel des stations à travers la province.

## Fonctionnalités

- **Carte interactive** : Carte Leaflet affichant toutes les stations avec les prix
- **Variations de prix** : Affiche les changements de prix sur 48 heures par station
- **Statistiques** : Graphiques historiques des prix avec filtrage par région (Chart.js)
- **Mises à jour automatiques** : Interroge les données en amont toutes les 5 minutes

## Stack technique

- **Backend** : Go avec `modernc.org/sqlite` (pilote SQLite 100% Go)
- **Frontend** : JavaScript pur, htmx pour l'interactivité
- **Carte** : Leaflet avec regroupement de marqueurs
- **Source des données** : [Régie de l'énergie Québec](https://regieessencequebec.ca)

## Démarrage rapide

```sh
# Entrer dans l'environnement de développement
nix develop
# ou
direnv allow

# Lancer le serveur
go run .

# Ou avec des paramètres personnalisés
PORT=8080 ESSENCE_DB=./essence.db go run .
```

Le serveur démarre à `http://localhost:8080` et redirige `/` vers `/map`.

## Routes

| Chemin | Description |
|--------|-------------|
| `/map` | Carte interactive avec toutes les stations |
| `/stats` | Graphiques historiques des prix |
| `/api/stations` | JSON : toutes les stations |
| `/api/stats` | JSON : historique mondial des prix |
| `/api/regions` | JSON : liste des régions |
| `/api/station-deltas` | JSON : variations de prix sur 48h |

## Variables d'environnement

| Variable | Défaut | Description |
|----------|--------|-------------|
| `PORT` | `8080` | Port d'écoute HTTP |
| `ESSENCE_DB` | `./essence.db` | Chemin de la base de données SQLite |

## Compilation

```sh
go build -o essence .
```

Ou via Nix :

```sh
nix build
./result/bin/essence
```

## Formatage et analyse statique

```sh
gofmt -w .
goimports -w .
go vet ./...
```

## Licence

MIT
