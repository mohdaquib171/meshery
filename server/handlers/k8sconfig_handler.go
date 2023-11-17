// Package handlers :  collection of handlers (aka "HTTP middleware")
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	mutil "github.com/layer5io/meshery/server/helpers/utils"
	"github.com/layer5io/meshery/server/machines"
	"github.com/layer5io/meshery/server/machines/kubernetes"

	"github.com/layer5io/meshery/server/models/connections"
	mcore "github.com/layer5io/meshery/server/models/meshmodel/core"
	meshmodelv1alpha1 "github.com/layer5io/meshkit/models/meshmodel/core/v1alpha1"

	// for GKE kube API authentication
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	"github.com/gofrs/uuid"
	"github.com/layer5io/meshery/server/helpers"
	"github.com/layer5io/meshery/server/models"
	"github.com/layer5io/meshery/server/models/pattern/core"
	putils "github.com/layer5io/meshery/server/models/pattern/utils"
	"github.com/layer5io/meshkit/models/events"
	meshmodel "github.com/layer5io/meshkit/models/meshmodel/registry"
	"github.com/layer5io/meshkit/utils"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// SaveK8sContextResponse - struct used as (json marshaled) response to requests for saving k8s contexts
type SaveK8sContextResponse struct {
	RegisteredContexts []models.K8sContext `json:"registered_contexts"`
	ConnectedContexts  []models.K8sContext `json:"connected_contexts"`
	IgnoredContexts    []models.K8sContext `json:"ignored_contexts"`
	ErroredContexts    []models.K8sContext `json:"errored_contexts"`
}

// K8SConfigHandler is used for persisting kubernetes config and context info
func (h *Handler) K8SConfigHandler(w http.ResponseWriter, req *http.Request, prefObj *models.Preference, user *models.User, provider models.Provider) {
	// if req.Method != http.MethodPost && req.Method != http.MethodDelete {
	// 	w.WriteHeader(http.StatusNotFound)
	// 	return
	// }
	if req.Method == http.MethodPost {
		h.addK8SConfig(user, prefObj, w, req, provider)
		return
	}
	if req.Method == http.MethodDelete {
		h.deleteK8SConfig(user, prefObj, w, req, provider)
		return
	}
}

// swagger:route POST /api/system/kubernetes SystemAPI idPostK8SConfig
// Handle POST request for Kubernetes Config
//
// Used to add kubernetes config to System
// responses:
// 	200: k8sConfigRespWrapper

// The function is called only when user uploads a kube config.
// Connections which have state as "registered" are the only new ones, hence the GraphQL K8sContext subscription only sends an update to UI if any connection has registered state.
// A registered connection might have been regsitered previously and is not required for K8sContext Subscription to notify, but this case is not considered here.
func (h *Handler) addK8SConfig(user *models.User, _ *models.Preference, w http.ResponseWriter, req *http.Request, provider models.Provider) {
	userID := uuid.FromStringOrNil(user.ID)

	token, ok := req.Context().Value(models.TokenCtxKey).(string)
	if !ok {
		err := ErrRetrieveUserToken(fmt.Errorf("failed to retrieve user token"))
		logrus.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	k8sConfigBytes, err := readK8sConfigFromBody(req)
	if err != nil {
		logrus.Error(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Flatten kubeconfig. If that fails, go ahead with non-flattened config file
	flattenedK8sConfig, err := helpers.FlattenMinifyKubeConfig(*k8sConfigBytes)
	if err == nil {
		k8sConfigBytes = &flattenedK8sConfig
	}

	saveK8sContextResponse := SaveK8sContextResponse{
		RegisteredContexts: make([]models.K8sContext, 0),
		ConnectedContexts:  make([]models.K8sContext, 0),
		IgnoredContexts:    make([]models.K8sContext, 0),
		ErroredContexts:    make([]models.K8sContext, 0),
	}

	eventBuilder := events.NewEvent().FromUser(userID).FromSystem(*h.SystemID).WithCategory("connection").WithAction("create").
		WithDescription("Kubernetes config uploaded.").WithSeverity(events.Informational)
	eventMetadata := map[string]interface{}{}
	contexts := models.K8sContextsFromKubeconfig(provider, user.ID, h.config.EventBroadcaster, *k8sConfigBytes, h.SystemID, eventMetadata)
	len := len(contexts)

	smInstanceTracker := h.ConnectionToStateMachineInstanceTracker
	smInstanceTracker.mx.Lock()
	for idx, ctx := range contexts {
		metadata := map[string]interface{}{}
		metadata["context"] = models.RedactCredentialsForContext(ctx)
		metadata["description"] = fmt.Sprintf("Connection established with context \"%s\" at %s", ctx.Name, ctx.Server)

		connection, err := provider.SaveK8sContext(token, *ctx)
		if err != nil {
			saveK8sContextResponse.ErroredContexts = append(saveK8sContextResponse.ErroredContexts, *ctx)
			metadata["description"] = fmt.Sprintf("Unable to establish connection with context \"%s\" at %s", ctx.Name, ctx.Server)
			metadata["error"] = err
		} else {
			ctx.ConnectionID = connection.ID.String()
			eventBuilder.ActedUpon(connection.ID)
			status := connection.Status
			machineCtx := &kubernetes.MachineCtx{
				K8sContext: *ctx,
				MesheryCtrlsHelper: h.MesheryCtrlsHelper,
				K8sCompRegHelper: h.K8sCompRegHelper,
				OperatorTracker: h.config.OperatorTracker,
				Provider: provider,
				K8scontextChannel: h.config.K8scontextChannel,
				EventBroadcaster: h.config.EventBroadcaster,
				RegistryManager: h.registryManager,
			}

			if status == connections.CONNECTED {
				saveK8sContextResponse.ConnectedContexts = append(saveK8sContextResponse.ConnectedContexts, *ctx)
				metadata["description"] = fmt.Sprintf("Connection already exists with Kubernetes context \"%s\" at %s", ctx.Name, ctx.Server)
			} else if status == connections.IGNORED {
				saveK8sContextResponse.IgnoredContexts = append(saveK8sContextResponse.IgnoredContexts, *ctx)
				metadata["description"] = fmt.Sprintf("Kubernetes context \"%s\" is set to ignored state.", ctx.Name)
			} else if status == connections.DISCOVERED {
				fmt.Println("test;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;")
				saveK8sContextResponse.RegisteredContexts = append(saveK8sContextResponse.RegisteredContexts, *ctx)
				metadata["description"] = fmt.Sprintf("Connection registered with kubernetes context \"%s\" at %s.", ctx.Name, ctx.Server)
			}

			err := InitializeMachineWithContext(
				machineCtx,
				req.Context(),
				connection.ID,
				smInstanceTracker,
				h.log,
				machines.StatusToEvent(status),
				false,
			)
			if err != nil {
				event := eventBuilder.FromSystem(*h.SystemID).ActedUpon(connection.ID).FromUser(userID).WithAction("management").WithCategory("system").WithSeverity(events.Critical).WithMetadata(map[string]interface{}{
					"error": err,
				}).WithDescription(fmt.Sprintf("Unable to transition to %s", status)).Build()
				_ = provider.PersistEvent(event)
				go h.config.EventBroadcaster.Publish(userID, event)
			}
		}

		eventMetadata[ctx.Name] = metadata

		if idx == len-1 {
			h.config.K8scontextChannel.PublishContext()
		}
	}
	smInstanceTracker.mx.Unlock()

	event := eventBuilder.WithMetadata(eventMetadata).Build()
	_ = provider.PersistEvent(event)
	go h.config.EventBroadcaster.Publish(userID, event)

	if err := json.NewEncoder(w).Encode(saveK8sContextResponse); err != nil {
		logrus.Error(models.ErrMarshal(err, "kubeconfig"))
		http.Error(w, models.ErrMarshal(err, "kubeconfig").Error(), http.StatusInternalServerError)
		return
	}
}

// swagger:route DELETE /api/system/kubernetes SystemAPI idDeleteK8SConfig
// Handle DELETE request for Kubernetes Config
//
// Used to delete kubernetes config to System
// responses:
// 	200:

func (h *Handler) deleteK8SConfig(_ *models.User, _ *models.Preference, w http.ResponseWriter, _ *http.Request, _ models.Provider) {
	// prefObj.K8SConfig = nil
	// err := provider.RecordPreferences(req, user.UserID, prefObj)
	// if err != nil {
	// 	logrus.Error(ErrRecordPreferences(err))
	// 	http.Error(w, ErrRecordPreferences(err).Error(), http.StatusInternalServerError)
	// 	return
	// }

	ctxID := "0" //To be replaced with actual context ID after multi context support
	go core.DeleteK8sWorkloads(ctxID)
	_, _ = w.Write([]byte("{}"))
}

// swagger:route POST /api/system/kubernetes/contexts SystemAPI idPostK8SContexts
// Handle POST requests for Kubernetes Context list
//
// Returns the context list for a given k8s config
// responses:
// 	200: k8sContextsRespWrapper

// GetContextsFromK8SConfig returns the context list for a given k8s config
func (h *Handler) GetContextsFromK8SConfig(w http.ResponseWriter, req *http.Request, _ *models.Preference, user *models.User, provider models.Provider) {

	k8sConfigBytes, err := readK8sConfigFromBody(req)
	if err != nil {
		logrus.Error(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userUUID := uuid.FromStringOrNil(user.ID)
	eventBuilder := events.NewEvent().FromUser(userUUID).FromSystem(*h.SystemID).WithCategory("connection").WithAction("discovered").
		WithDescription("Kubernetes config uploaded.").WithSeverity(events.Informational)

	eventMetadata := map[string]interface{}{}

	contexts := models.K8sContextsFromKubeconfig(provider, user.ID, h.config.EventBroadcaster, *k8sConfigBytes, h.SystemID, eventMetadata)

	event := eventBuilder.WithMetadata(eventMetadata).Build()
	_ = provider.PersistEvent(event)
	go h.config.EventBroadcaster.Publish(userUUID, event)

	err = json.NewEncoder(w).Encode(contexts)
	if err != nil {
		logrus.Error(models.ErrMarshal(err, "kube-context"))
		http.Error(w, models.ErrMarshal(err, "kube-context").Error(), http.StatusInternalServerError)
		return
	}
}

// swagger:route GET /api/system/kubernetes/ping?connection_id={id} SystemAPI idGetKubernetesPing
// Handle GET request for Kubernetes ping
//
// Fetches server version to simulate ping
// responses:
// 	200:

// KubernetesPingHandler - fetches server version to simulate ping
func (h *Handler) KubernetesPingHandler(w http.ResponseWriter, req *http.Request, _ *models.Preference, _ *models.User, provider models.Provider) {
	token, ok := req.Context().Value(models.TokenCtxKey).(string)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "failed to get the token for the user")
		return
	}

	connectionID := req.URL.Query().Get("connection_id")
	if connectionID != "" {
		// Get the context associated with this ID
		k8sContext, err := provider.GetK8sContext(token, connectionID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "failed to get kubernetes context for the given ID")
			return
		}

		// Create handler for the context
		kubeclient, err := k8sContext.GenerateKubeHandler()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "failed to get kubernetes config for the user")
			return
		}
		version, err := kubeclient.KubeClient.ServerVersion()
		if err != nil {
			logrus.Error(ErrKubeVersion(err))
			http.Error(w, ErrKubeVersion(err).Error(), http.StatusInternalServerError)
			return
		}
		if err = json.NewEncoder(w).Encode(map[string]string{
			"server_version": version.String(),
		}); err != nil {
			err = errors.Wrap(err, "unable to marshal the payload")
			logrus.Error(models.ErrMarshal(err, "kube-server-version"))
			http.Error(w, models.ErrMarshal(err, "kube-server-version").Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Error(w, "Empty contextID. Pass the context ID(in query parameter \"context\") of the kuberenetes to be pinged", http.StatusBadRequest)
}

// swagger:route POST /api/system/kubernetes/register SystemAPI idPostK8SRegistration
// Handle registration request for Kubernetes components
//
// Used to register Kubernetes components to Meshery from a kubeconfig file
// responses:
//
//		202:
//	 400:
//	 500:
func (h *Handler) K8sRegistrationHandler(w http.ResponseWriter, req *http.Request, _ *models.Preference, user *models.User, provider models.Provider) {
	k8sConfigBytes, err := readK8sConfigFromBody(req)
	if err != nil {
		logrus.Error(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	contexts := models.K8sContextsFromKubeconfig(provider, user.ID, h.config.EventBroadcaster, *k8sConfigBytes, h.SystemID, map[string]interface{}{}) // here we are not concerned for the events becuase inside the middleware the contexts would have been verified.
	h.K8sCompRegHelper.UpdateContexts(contexts).RegisterComponents(contexts, []models.K8sRegistrationFunction{RegisterK8sMeshModelComponents}, h.registryManager, h.config.EventBroadcaster, provider, user.ID, false)
	if _, err = w.Write([]byte(http.StatusText(http.StatusAccepted))); err != nil {
		logrus.Error(ErrWriteResponse)
		logrus.Error(err)
		http.Error(w, ErrWriteResponse.Error(), http.StatusInternalServerError)
	}
}

func (h *Handler) DiscoverK8SContextFromKubeConfig(userID string, token string, prov models.Provider) ([]*models.K8sContext, error) {
	var contexts []*models.K8sContext
	userUUID := uuid.FromStringOrNil(userID)

	// Get meshery instance ID
	mid, ok := viper.Get("INSTANCE_ID").(*uuid.UUID)
	if !ok {
		return contexts, models.ErrMesheryInstanceID
	}

	// Attempt to get kubeconfig from the filesystem
	if h.config == nil {
		return contexts, ErrInvalidK8SConfigNil
	}
	kubeconfigSource := fmt.Sprintf("file://%s", filepath.Join(h.config.KubeConfigFolder, "config"))
	data, err := utils.ReadFileSource(kubeconfigSource)
	
	eventBuilder := events.NewEvent().FromUser(userUUID).FromSystem(*h.SystemID).WithCategory("connection").WithAction("create").
		WithDescription(fmt.Sprintf("Kubernetes config imported from %s.", kubeconfigSource)).WithSeverity(events.Informational)
	eventMetadata := map[string]interface{}{}
	metadata := map[string]interface{}{}
	if err != nil {
		// Could be an in-cluster deployment
		ctxName := "in-cluster"

		cc, err := models.NewK8sContextFromInClusterConfig(ctxName, mid)
		if err != nil {
			metadata["description"] = "Failed to import in-cluster kubeconfig."
			metadata["error"] = err
			logrus.Warn("failed to generate in cluster context: ", err)
			return contexts, err
		}
		if cc == nil {
			metadata["description"] = "No contexts detected in the in-cluster kubeconfig."
			err := fmt.Errorf("nil context generated from in cluster config")
			logrus.Warn(err)
			return contexts, err
		}
		cc.DeploymentType = "in_cluster"
		conn, err := prov.SaveK8sContext(token, *cc)
		if err != nil {
			metadata["description"] = fmt.Sprintf("Unable to establish connection with context \"%s\" at %s", cc.Name, cc.Server)
			metadata["error"] = err
			logrus.Warn("failed to save the context for incluster: ", err)
			return contexts, err
		}
		h.log.Debug(conn)
		// updatedConnection, statusCode, err := prov.UpdateConnectionStatusByID(token, conn.ID, connections.CONNECTED)
		// if err != nil || statusCode != http.StatusOK {
		// 	logrus.Warn("failed to update connection status for connection id", conn.ID, "to", connections.CONNECTED)
		// 	logrus.Debug("connection: ", updatedConnection)
		// 	metadata["description"] = fmt.Sprintf("Unable to establish connection with context \"%s\" at %s", cc.Name, cc.Server)
		// 	metadata["error"] = err
		// 	return contexts, err
		// }
		// h.config.K8scontextChannel.PublishContext()
		cc.ConnectionID = conn.ID.String()
		contexts = append(contexts, cc)
		metadata["context"] = models.RedactCredentialsForContext(cc)
		eventMetadata["in-cluster"] = metadata
		event := eventBuilder.WithMetadata(eventMetadata).Build()
		_ = prov.PersistEvent(event)
		go h.config.EventBroadcaster.Publish(userUUID, event)
		return contexts, nil
	}

	cfg, err := helpers.FlattenMinifyKubeConfig([]byte(data))
	if err != nil {
		return contexts, err
	}
	
	ctxs := models.K8sContextsFromKubeconfig(prov, userID, h.config.EventBroadcaster, cfg, mid, eventMetadata)

	// Do not persist the generated contexts
	// consolidate this func and addK8sConfig. In this we explicitly updated status as well as this func perfomr greeedy upload so while consolidating make sure to handle the case.
	for _, ctx := range ctxs {
		metadata := map[string]interface{}{}
		metadata["context"] = models.RedactCredentialsForContext(ctx)
		metadata["description"] = fmt.Sprintf("K8S context \"%s\" discovered with cluster at %s", ctx.Name, ctx.Server)
		metadata["description"] = fmt.Sprintf("Connection established with context \"%s\" at %s", ctx.Name, ctx.Server)
		ctx.DeploymentType = "out_of_cluster"
		conn, err := prov.SaveK8sContext(token, *ctx)
		if err != nil {
			logrus.Warn("failed to save the context: ", err)
			metadata["description"] = fmt.Sprintf("Unable to establish connection with context \"%s\" at %s", ctx.Name, ctx.Server)
			metadata["error"] = err
			continue
		}
		ctx.ConnectionID = conn.ID.String()
		h.log.Debug(conn)
		// updatedConnection, statusCode, err := prov.UpdateConnectionStatusByID(token, conn.ID, connections.CONNECTED)
		// if err != nil || statusCode != http.StatusOK {
		// 	logrus.Warn("failed to update connection status for connection id", conn.ID, "to", connections.CONNECTED)
		// 	logrus.Debug("connection: ", updatedConnection)

		// 	metadata["description"] = fmt.Sprintf("Unable to establish connection with context \"%s\" at %s", ctx.Name, ctx.Server)
		// 	metadata["error"] = err
		// 	continue
		// }

		contexts = append(contexts, ctx)
	}
	// if len(contexts) > 0 {
	// 	h.config.K8scontextChannel.PublishContext()
	// }
	event := eventBuilder.WithMetadata(eventMetadata).Build()
	_ = prov.PersistEvent(event)
	go h.config.EventBroadcaster.Publish(userUUID, event)

	return contexts, nil
}

func RegisterK8sMeshModelComponents(provider *models.Provider, _ context.Context, config []byte, ctxID string, connectionID string, userID string, mesheryInstanceID uuid.UUID, reg *meshmodel.RegistryManager, ec *models.Broadcast, ctxName string) (err error) {
	connectionUUID := uuid.FromStringOrNil(connectionID)
	userUUID := uuid.FromStringOrNil(userID)

	man, err := mcore.GetK8sMeshModelComponents(config)
	if err != nil {
		return ErrCreatingKubernetesComponents(err, ctxID)
	}
	if man == nil {
		return ErrCreatingKubernetesComponents(errors.New("generated components are nil"), ctxID)
	}
	count := 0
	for _, c := range man {
		writeK8sMetadata(&c, reg)
		err = reg.RegisterEntity(meshmodel.Host{
			Hostname: "kubernetes",
			Metadata: ctxID,
		}, c)
		count++
	}
	event := events.NewEvent().ActedUpon(connectionUUID).WithCategory("kubernetes_components").WithAction("registration").FromSystem(mesheryInstanceID).FromUser(userUUID).WithSeverity(events.Informational).WithDescription(fmt.Sprintf("%d Kubernetes components registered for %s", count, ctxName)).WithMetadata(map[string]interface{}{
		"doc": "https://docs.meshery.io/tasks/lifecycle-management",
	}).Build()

	_ = (*provider).PersistEvent(event)
	ec.Publish(userUUID, event)
	return
}

const k8sMeshModelPath = "../meshmodel/kubernetes/model_template.json"

var k8sMeshModelMetadata = make(map[string]interface{})

func writeK8sMetadata(comp *meshmodelv1alpha1.ComponentDefinition, reg *meshmodel.RegistryManager) {
	ent, _, _ := reg.GetEntities(&meshmodelv1alpha1.ComponentFilter{
		Name:       comp.Kind,
		APIVersion: comp.APIVersion,
	})
	//If component was not available in the registry, then use the generic model level metadata
	if len(ent) == 0 {
		putils.MergeMaps(comp.Metadata, k8sMeshModelMetadata)
		mutil.WriteSVGsOnFileSystem(comp)
	} else {
		existingComp, ok := ent[0].(meshmodelv1alpha1.ComponentDefinition)
		if !ok {
			putils.MergeMaps(comp.Metadata, k8sMeshModelMetadata)
			return
		}
		putils.MergeMaps(comp.Metadata, existingComp.Metadata)
		comp.Model = existingComp.Model
	}
}

// Caches k8sMeshModel metadatas in memory to use at the time of dynamic k8s component generation
func init() {
	f, err := os.Open(filepath.Join(k8sMeshModelPath))
	if err != nil {
		return
	}
	byt, err := io.ReadAll(f)
	if err != nil {
		return
	}
	m := make(map[string]interface{})
	err = json.Unmarshal(byt, &m)
	if err != nil {
		return
	}
	k8sMeshModelMetadata = m
}

func readK8sConfigFromBody(req *http.Request) (*[]byte, error) {
	_ = req.ParseMultipartForm(1 << 20)

	k8sfile, _, err := req.FormFile("k8sfile")
	if err != nil {
		return nil, ErrFormFile(err)
	}
	defer func() {
		_ = k8sfile.Close()
	}()

	k8sConfigBytes, err := io.ReadAll(k8sfile)
	if err != nil {
		return nil, ErrReadConfig(err)
	}
	return &k8sConfigBytes, nil
}


// func buildK8sConnectionFromContext(context models.K8sContext) (conn *connections.Connection) {
// 	metadata := map[string]string{
// 		"id":                   context.ID,
// 		"server":               context.Server,
// 		"meshery_instance_id":  context.MesheryInstanceID.String(),
// 		"deployment_type":      context.DeploymentType,
// 		"version":              context.Version,
// 		"name":                 context.Name,
// 		"kubernetes_server_id": "", // assign afterwards
// 	}
	
// 	conn = &connections.Connection{
		
// 	}

// }
// func writeDefK8sOnFileSystem(def string, path string) {
// 	err := ioutil.WriteFile(path, []byte(def), 0777)
// 	if err != nil {
// 		fmt.Println("err def: ", err.Error())
// 	}
// }

// func writeSchemaK8sFileSystem(schema string, path string) {
// 	err := ioutil.WriteFile(path, []byte(schema), 0777)
// 	if err != nil {
// 		fmt.Println("err schema: ", err.Error())
// 	}
// }
