# Tasks — Open Source Production Readiness

Checklist of everything needed to bring `saunafs-operator` to a production-ready open source state.
Items are grouped by priority. Check boxes as work is completed.

---

## 🔴 Priorité haute — Fonctionnalités bloquantes

### 1. Status et Conditions du CRD
- [x] Définir des champs de status dans `SaunaFSClusterStatus` : conditions `Ready`, nombre de chunkservers actifs, total.
- [x] Mettre à jour le statut dans le contrôleur (`r.Status().Patch(...)`) à chaque réconciliation.
- [x] Utiliser le pattern `metav1.Condition` (Type, Status, Reason, Message, ObservedGeneration).

### 2. Persistance des métadonnées du master
- [ ] Remplacer l'`emptyDir` du master par un PVC ou une option `hostPath` configurable dans `MasterSpec`.
- [ ] Ajouter un champ `Storage MountPath` dans `MasterSpec` pour le répertoire `/var/lib/saunafs`.
- [ ] Documenter les implications d'une perte de données si `emptyDir` est conservé.

### 3. CI/CD — GitHub Actions
- [ ] Créer `.github/workflows/ci.yml` : build Go + `make test` + `make lint` sur chaque PR et push sur `main`.
- [ ] Créer `.github/workflows/release.yml` : build et push de l'image sur `ghcr.io/henres/saunafs-operator`, création de release GitHub, publication du chart Helm.
- [ ] Configurer les secrets nécessaires (`GHCR_TOKEN` ou `GITHUB_TOKEN`).

### 4. Images Docker publiées
- [ ] Fixer l'image de l'opérateur dans le `Makefile` : `IMG ?= ghcr.io/henres/saunafs-operator:latest`.
- [ ] Publier les images SaunaFS (`saunafs-master`, `saunafs-chunkserver`, etc.) depuis `leil-io/saunafs-container` sur un registry public accessible, ou documenter clairement comment les builder avec `hack/build-saunafs-images.sh`.
- [ ] Utiliser des tags versionnés (pas `latest`) dans les valeurs par défaut du CRD.

---

## 🟠 Priorité moyenne — Robustesse et qualité

### 5. Gestion des erreurs et requeue
- [ ] Mettre à jour le statut en cas d'erreur de réconciliation (condition `Ready=False` avec message d'erreur).
- [ ] Ajouter un `Result{RequeueAfter: 30 * time.Second}` pour détecter le drift périodiquement.

### 6. Finalizers pour le cleanup
- [ ] Ajouter un finalizer sur `SaunaFSCluster` pour gérer la suppression propre.
- [ ] Nettoyer les PVCs dynamiquement provisionnés lors de la suppression du cluster.

### 7. Webhook de validation
- [ ] Implémenter un `ValidatingWebhookConfiguration` pour vérifier :
  - Unicité des noms de chunkservers dans `chunk.servers`.
  - Absence de `mountPath` dupliqués sur le même nœud.
  - Valeurs de `NodePort` dans la plage 30000–32767.
- [ ] Configurer cert-manager ou le manager pour injecter le certificat TLS du webhook.

### 8. Tests unitaires enrichis
- [ ] Compléter `saunafscluster_controller_test.go` avec des assertions sur les ressources créées :
  - DaemonSet master créé avec les bons labels, image, volumes.
  - StatefulSets chunkservers créés pour chaque entrée dans `chunk.servers`.
  - Services créés avec les bons types et ports.
- [ ] Tester les cas de mise à jour (changement d'image, ajout d'un chunkserver).
- [ ] Tester la suppression d'un chunkserver (ressource orpheline nettoyée).

### 9. Tests e2e fonctionnels
- [ ] Documenter le setup kind dans le README (`hack/kind-config.yaml`).
- [ ] Ajouter des tests e2e qui vérifient que les pods passent en état `Running` après création d'un `SaunaFSCluster`.
- [ ] Ajouter un test de montage effectif du filesystem SaunaFS depuis un pod client.

### 10. Versioning et processus de release
- [ ] Créer `CHANGELOG.md` et adopter le format [Keep a Changelog](https://keepachangelog.com/).
- [ ] Tagguer la version initiale `v0.1.0` une fois les items bloquants résolus.
- [ ] Générer et committer `dist/install.yaml` à chaque release via `make build-installer`.
- [ ] Publier le chart Helm (OCI registry ou GitHub Pages avec `chart-releaser`).

---

## 🟡 Priorité basse — Bonnes pratiques et expérience développeur

### 11. Copyright headers
- [ ] Renseigner le nom de l'auteur/organisation dans `hack/boilerplate.go.txt` (remplacer `Copyright 2026.`).
- [ ] Regénérer tous les fichiers avec `make generate` pour propager le header.

### 12. README complet
- [ ] Ajouter un diagramme d'architecture (composants master, chunkservers, CSI, NFS).
- [ ] Documenter tous les champs du CRD avec exemples (ou pointer vers une référence générée).
- [ ] Ajouter un guide de démarrage rapide avec `kind` step-by-step.
- [ ] Lister les versions Kubernetes testées et compatibles.
- [ ] Clarifier les prérequis d'images (lien vers `leil-io/saunafs-container`).

### 13. CONTRIBUTING.md
- [ ] Créer `CONTRIBUTING.md` avec : branching strategy, convention de commit, processus de PR/review, DCO ou CLA si applicable.

### 14. Liveness et Readiness probes
- [ ] Ajouter des probes de santé sur les containers master et chunkserver dans le contrôleur (au minimum `tcpSocket` sur le port principal).

### 15. Métriques Prometheus
- [ ] Exposer des métriques custom via le registry controller-runtime :
  - Nombre de clusters réconciliés.
  - Nombre d'erreurs de réconciliation.
  - Durée de réconciliation (histogram).

### 16. Plan de stabilisation de l'API
- [ ] Documenter les conditions de graduation de `v1alpha1` → `v1beta1` (champs établis stables, webhooks en place, tests e2e passants).
- [ ] Ajouter un commentaire `+kubebuilder:deprecation` sur les champs susceptibles de changer.

---

## 🔵 Améliorations futures — v2

### 17. Découverte automatique des disques

Le modèle actuel est **déclaratif** : l'utilisateur liste explicitement les chunkservers et leurs volumes dans le CRD. C'est intentionnel et suffisant pour une v1.

En v2, on pourrait implémenter la découverte automatique à la façon de Sarkan/Rook :

- [ ] Déployer un **DaemonSet de découverte** sur chaque nœud qui liste les block devices disponibles (`lsblk`, udev) et filtre ceux qui ne sont pas montés ni partitionnés.
- [ ] Remonter les disques découverts via une annotation sur le `Node` ou une CR dédiée par nœud (ex: `SaunaFSNode`).
- [ ] Calculer automatiquement les chunkservers à créer en fonction d'une politique configurable (`desiredMinimumChunkserverCount`, taille minimale des disques, labels de nœud).
- [ ] Gérer l'idempotence : ne jamais reformater un disque déjà utilisé par SaunaFS.
- [ ] Gérer la disparition d'un disque (chunkserver à marquer dégradé plutôt que supprimé silencieusement).

> **Note** : cette feature nécessite des pods privilégiés avec accès à `/dev`. À traiter avec une analyse de sécurité dédiée.

---

## ✅ Déjà en place

- Licence Apache 2.0
- Structure Kubebuilder standard
- RBAC markers complets dans le contrôleur
- Chart Helm de base
- Script de build des images SaunaFS (`hack/build-saunafs-images.sh`)
- Support NFS-Ganesha, CSI, WebUI, Expose dans le CRD
- `createOrUpdate*` helpers pour éviter les conflits de réconciliation
- `SetupWithManager` avec `Owns()` sur toutes les ressources enfants
