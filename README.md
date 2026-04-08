# Prix du gaz

Carte des prix d'essence au Québec affichant les prix en temps réel des stations à travers la province.

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
PORT=8080 PRIXDUGAZ_DB=./prixdugaz.db go run .
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
| `PRIXDUGAZ_DB` | `./prixdugaz.db` | Chemin de la base de données SQLite |

## Compilation

```sh
go build -o prixdugaz .
```

Ou via Nix :

```sh
nix build
./result/bin/prixdugaz
```

## Formatage et analyse statique

```sh
gofmt -w .
goimports -w .
go vet ./...
```

## Déploiement sur NixOS

Le projet fournit un module NixOS via le flake. Ajoutez-le à votre configuration :

**1. Ajoutez le flake comme entrée (`flake.nix`) :**

```nix
inputs.prixdugaz.url = "github:Polensky/prixdugaz";
```

**2. Importez le module NixOS et activez le service :**

```nix
imports = [ inputs.prixdugaz.nixosModules.default ];

services.prixdugaz = {
  enable = true;
  port = 8080;          # optionnel, 8080 par défaut
  openFirewall = true;  # optionnel, ouvre le port dans le pare-feu
};
```

**Options disponibles :**

| Option | Défaut | Description |
|--------|--------|-------------|
| `enable` | `false` | Active le service |
| `port` | `8080` | Port d'écoute HTTP |
| `dataDir` | `/var/lib/prixdugaz` | Répertoire de la base de données SQLite |
| `openFirewall` | `false` | Ouvre le port dans le pare-feu |
| `package` | flake par défaut | Paquet Prix du gaz à utiliser |


## Licence

MIT
