package tracker

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/emirpasic/gods/sets/hashset"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	argocdclientset "github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/v3/util/db"
	"github.com/argoproj/argo-cd/v3/util/settings"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/anandf/resource-tracker/pkg/resourcegraph"
	"k8s.io/client-go/tools/clientcmd"
	clientauthapi "k8s.io/client-go/tools/clientcmd/api"
)

// ResourceTracker handles the analysis of ArgoCD application resources
type ResourceTracker struct {
	db                  db.ArgoDB
	argoClient          argocdclientset.Interface
	queryServer         *graph.QueryServer
	resourceMapperStore map[string]*resourcegraph.ResourceMapper
	kubeconfig          string
	//shared cache across ALL clusters: parentKey -> set(childKey)
	sharedRelationsCache map[string]*hashset.Set
	cacheMu              sync.RWMutex
}

// DirectResources represents direct resources discovered from Application.status
// It includes both detailed resource infos (for graph queries) and compact keys
// (for resource-graph relations traversal).
type DirectResources struct {
	Infos []graph.ResourceInfo
	Keys  map[string]struct{}
}

// NewResourceTracker creates a new resource tracker instance
func NewResourceTracker(argoClient argocdclientset.Interface, clientset kubernetes.Interface, controllerNamespace string) *ResourceTracker {

	settingsMgr := settings.NewSettingsManager(context.Background(), clientset, controllerNamespace)
	dbInstance := db.NewDB(controllerNamespace, settingsMgr, clientset)

	return &ResourceTracker{
		argoClient:           argoClient,
		db:                   dbInstance,
		resourceMapperStore:  make(map[string]*resourcegraph.ResourceMapper),
		sharedRelationsCache: make(map[string]*hashset.Set),
	}
}

// InitializeQueryServer initializes the graph query server with cluster configuration
func (rt *ResourceTracker) InitializeQueryServer(restConfig *rest.Config) error {
	queryServer, err := graph.NewQueryServer(restConfig, "label")
	if err != nil {
		return fmt.Errorf("failed to initialize query server: %w", err)
	}
	rt.queryServer = queryServer
	return nil
}

// AnalyzeAllApplications analyzes all ArgoCD applications in the specified namespace
func (rt *ResourceTracker) AnalyzeAllApplications(namespace string) error {
	apps, err := rt.argoClient.ArgoprojV1alpha1().Applications(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list applications: %w", err)
	}

	allRelationships := make(map[string]*hashset.Set)
	for _, app := range apps.Items {
		keys := make(map[string]struct{})
		// status.resources
		for _, res := range app.Status.Resources {
			apiVersion := res.Version
			if res.Group != "" {
				apiVersion = fmt.Sprintf("%s/%s", res.Group, res.Version)
			}
			key := kube.GetResourceKey(apiVersion, res.Kind)
			keys[key] = struct{}{}
		}
		// Optional: parse ExcludedResourceWarning
		rx := regexp.MustCompile(`([A-Za-z0-9.-]*)/([A-Za-z0-9.]+) ([A-Za-z0-9-_.]+)`) // group/kind name
		for _, cond := range app.Status.Conditions {
			if strings.EqualFold(cond.Type, "ExcludedResourceWarning") {
				matches := rx.FindStringSubmatch(cond.Message)
				if len(matches) == 4 {
					group := matches[1]
					kind := matches[2]
					apiVersion := "v1"
					if group != "" {
						apiVersion = fmt.Sprintf("%s/%s", group, "v1")
					}
					key := kube.GetResourceKey(apiVersion, kind)
					keys[key] = struct{}{}
				}
			}
		}
		// Convert keys to inclusions
		for k := range keys {
			parts := strings.SplitN(k, "_", 2)
			if len(parts) != 2 {
				continue
			}
			group := parts[0]
			if group == "core" {
				group = ""
			}
			kind := parts[1]
			apiGroup := group
			if _, ok := allRelationships[apiGroup]; !ok {
				allRelationships[apiGroup] = hashset.New()
			}
			allRelationships[apiGroup].Add(kind)
		}
	}

	return rt.outputInclusionsForAll(allRelationships)
}

// outputInclusionsForAll prints resource.inclusions-style YAML grouped by API group for all apps
func (rt *ResourceTracker) outputInclusionsForAll(allRelationships map[string]*hashset.Set) error {
	groupToKinds := make(map[string]map[string]struct{})
	for group, kinds := range allRelationships {
		if _, ok := groupToKinds[group]; !ok {
			groupToKinds[group] = make(map[string]struct{})
		}
		for _, v := range kinds.Values() {
			kind, _ := v.(string)
			groupToKinds[group][kind] = struct{}{}
		}
	}
	rt.printInclusions(groupToKinds)
	return nil
}

// AnalyzeWithResourceGraph computes inclusions using status-based direct kinds and the resource graph
func (rt *ResourceTracker) AnalyzeWithResourceGraph(config *rest.Config, apps []*v1alpha1.Application) error {
	fmt.Printf("AnalyzeWithResourceGraph: start apps=%d\n", len(apps))
	groupToKinds := make(map[string]map[string]struct{})
	addKey := func(key string) {
		parts := strings.SplitN(key, "_", 2)
		if len(parts) != 2 {
			return
		}
		group := parts[0]
		if group == "core" {
			group = ""
		}
		kind := parts[1]
		if _, ok := groupToKinds[group]; !ok {
			groupToKinds[group] = make(map[string]struct{})
		}
		groupToKinds[group][kind] = struct{}{}
	}

	type result struct{ err error }
	jobs := make(chan *v1alpha1.Application)
	results := make(chan result)
	// Bounded workers
	workerCount := 4
	var wg sync.WaitGroup
	report := func(err error) { results <- result{err: err} }
	wg.Add(workerCount)
	for range workerCount {
		go rt.analyzeWithResourceGraphWorker(jobs, addKey, report, &wg)
	}
	go func() {
		for _, app := range apps {
			jobs <- app
		}
		close(jobs)
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	for r := range results {
		if r.err != nil {
			return r.err
		}
	}
	rt.printInclusions(groupToKinds)
	return nil
}

// analyzeWithResourceGraphWorker reads Applications from jobs and processes them,
// reporting errors via report and adding discovered keys through addKey.
func (rt *ResourceTracker) analyzeWithResourceGraphWorker(
	jobs <-chan *v1alpha1.Application,
	addKey func(string),
	report func(error),
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	for app := range jobs {
		fmt.Printf("AnalyzeWithResourceGraph: building direct resources for app=%s\n", app.GetName())
		dr, err := rt.GetDirectResourcesFromStatus(app)
		if err != nil {
			report(err)
			continue
		}
		fmt.Printf("AnalyzeWithResourceGraph: direct keys=%d infos=%d\n", len(dr.Keys), len(dr.Infos))
		if len(rt.resourceMapperStore) == 0 {
			report(fmt.Errorf("no destination clusters synced; ensure Applications have valid .spec.destination and Argo CD has access"))
			continue
		}

		// Resolve destination server once per app
		server := app.Spec.Destination.Server
		if server == "" {
			var err error
			if app.Spec.Destination.Name == "" {
				report(fmt.Errorf("both destination server and name are empty"))
				continue
			}
			server, err = getClusterServerByName(context.Background(), rt.db, app.Spec.Destination.Name)
			if err != nil {
				report(fmt.Errorf("error getting cluster: %w", err))
				continue
			}
		}
		// Check if any direct key is missing in cache
		rt.cacheMu.RLock()
		syncRequired := false
		for k := range dr.Keys {
			if _, exists := rt.sharedRelationsCache[k]; !exists {
				syncRequired = true
				break
			}
		}
		rt.cacheMu.RUnlock()
		if syncRequired {
			fmt.Printf("AnalyzeWithResourceGraph: syncing cache host=%s\n", server)
			rt.ensureSyncedSharedCacheOnHost(context.Background(), server)
			// if the direct keys are not in the cache, add them to the cache as left nodes
			rt.cacheMu.Lock()
			for k := range dr.Keys {
				if _, exists := rt.sharedRelationsCache[k]; !exists {
					rt.sharedRelationsCache[k] = hashset.New()
				}
			}
			rt.cacheMu.Unlock()
		}

		visited := make(map[string]struct{})
		queue := make([]string, 0, len(dr.Keys))
		for k := range dr.Keys {
			queue = append(queue, k)
		}
		for len(queue) > 0 {
			curr := queue[0]
			queue = queue[1:]
			if _, seen := visited[curr]; seen {
				continue
			}
			visited[curr] = struct{}{}
			addKey(curr)
			rt.cacheMu.RLock()
			children, ok := rt.sharedRelationsCache[curr]
			rt.cacheMu.RUnlock()
			if ok {
				for _, v := range children.Values() {
					if childKey, ok := v.(string); ok {
						queue = append(queue, childKey)
					}
				}
			}
		}
		report(nil)
	}
}

// AnalyzeWithGraph computes inclusions using @graph query server, starting from status-based direct resources
func (rt *ResourceTracker) AnalyzeWithGraph(config *rest.Config, apps []*v1alpha1.Application) error {
	if err := rt.InitializeQueryServer(config); err != nil {
		return err
	}
	fmt.Printf("AnalyzeWithGraph: start apps=%d\n", len(apps))
	dr := &DirectResources{
		Infos: make([]graph.ResourceInfo, 0),
		Keys:  make(map[string]struct{}),
	}
	// Build direct resource infos from Application.status
	for _, app := range apps {
		appDr, err := rt.GetDirectResourcesFromStatus(app)
		if err != nil {
			return err
		}
		dr.Infos = append(dr.Infos, appDr.Infos...)
		for k := range appDr.Keys {
			dr.Keys[k] = struct{}{}
		}
	}
	fmt.Printf("AnalyzeWithGraph: direct infos=%d keys=%d\n", len(dr.Infos), len(dr.Keys))

	// Aggregate all discovered child resources
	allChildResources := make(graph.ResourceInfoSet)
	// Also include direct kinds themselves
	groupToKinds := make(map[string]map[string]struct{})
	addKind := func(apiVersion, kind string) {
		group := ""
		if strings.Contains(apiVersion, "/") {
			group = strings.SplitN(apiVersion, "/", 2)[0]
		}
		if _, ok := groupToKinds[group]; !ok {
			groupToKinds[group] = make(map[string]struct{})
		}
		groupToKinds[group][kind] = struct{}{}
	}

	for _, info := range dr.Infos {
		// include direct kinds
		apiVersion := info.APIVersion
		if apiVersion == "" {
			apiVersion = "v1"
		}
		addKind(apiVersion, info.Kind)

		children, err := rt.queryServer.GetNestedChildResources(&info)
		if err != nil {
			continue
		}
		for c := range children {
			allChildResources[c] = graph.Void{}
		}
	}

	for child := range allChildResources {
		apiVersion := child.APIVersion
		if apiVersion == "" {
			apiVersion = "v1"
		}
		addKind(apiVersion, child.Kind)
	}

	// Emit inclusions
	rt.printInclusions(groupToKinds)
	return nil
}

// GetDirectResourcesFromStatus returns both detailed infos and compact keys
// derived from Application.status resources and conditions.
func (rt *ResourceTracker) GetDirectResourcesFromStatus(app *v1alpha1.Application) (*DirectResources, error) {
	fmt.Printf("GetDirectResourcesFromStatus: app=%s resources=%d conditions=%d\n", app.GetName(), len(app.Status.Resources), len(app.Status.Conditions))
	dr := &DirectResources{
		Infos: make([]graph.ResourceInfo, 0),
		Keys:  make(map[string]struct{}),
	}
	rx := regexp.MustCompile(`([A-Za-z0-9.-]*)/([A-Za-z0-9.]+) ([A-Za-z0-9-_.]+)`) // group/kind name

	add := func(kind, apiVersion, name, namespace string) {
		dr.Infos = append(dr.Infos, graph.ResourceInfo{
			Kind:       kind,
			APIVersion: apiVersion,
			Name:       name,
			Namespace:  namespace,
		})
		key := kube.GetResourceKey(apiVersion, kind)
		dr.Keys[key] = struct{}{}
	}

	for _, res := range app.Status.Resources {
		apiVersion := res.Version
		if res.Group != "" {
			apiVersion = fmt.Sprintf("%s/%s", res.Group, res.Version)
		}
		add(res.Kind, apiVersion, res.Name, res.Namespace)
	}
	for _, cond := range app.Status.Conditions {
		if strings.EqualFold(cond.Type, "ExcludedResourceWarning") {
			matches := rx.FindStringSubmatch(cond.Message)
			if len(matches) == 4 {
				group := matches[1]
				kind := matches[2]
				name := matches[3]
				apiVersion := "v1"
				if group != "" {
					apiVersion = fmt.Sprintf("%s/%s", group, "v1")
				}
				add(kind, apiVersion, name, "")
			}
		}
	}

	//sync resource mapper
	err := rt.syncResourceMapper(app)
	if err != nil {
		return nil, fmt.Errorf("failed to sync resource mapper: %w", err)
	}

	return dr, nil
}

// printInclusions prints resource.inclusions YAML from a group->kinds map
func (rt *ResourceTracker) printInclusions(groupToKinds map[string]map[string]struct{}) {
	// Collect & sort groups
	groups := make([]string, 0, len(groupToKinds))
	for g := range groupToKinds {
		groups = append(groups, g)
	}
	sort.Strings(groups)

	fmt.Println("resource.inclusions: |")
	for _, g := range groups {
		// Collect & sort kinds within the group
		kindsSet := groupToKinds[g]
		kinds := make([]string, 0, len(kindsSet))
		for k := range kindsSet {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)

		fmt.Println("  - apiGroups:")
		if g == "" || g == "core" {
			fmt.Println("    - \"\"")
		} else {
			fmt.Printf("    - %s\n", g)
		}
		fmt.Println("    kinds:")
		for _, k := range kinds {
			fmt.Printf("    - %s\n", k)
		}
		fmt.Println("    clusters:")
		fmt.Println("    - \"*\"")
	}
}

func (rt *ResourceTracker) syncResourceMapper(app *v1alpha1.Application) error {
	var err error
	server := app.Spec.Destination.Server
	if server == "" {
		if app.Spec.Destination.Name == "" {
			return fmt.Errorf("both destination server and name are empty")
		}
		server, err = getClusterServerByName(context.Background(), rt.db, app.Spec.Destination.Name)
		if err != nil {
			return fmt.Errorf("error getting cluster: %w", err)
		}
	}
	cluster, err := rt.db.GetCluster(context.Background(), server)
	if err != nil {
		return fmt.Errorf("resolve destination cluster: %w", err)
	}
	restCfg, err := restConfigFromCluster(cluster, rt.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create rest config: %w", err)
	}
	if _, exists := rt.resourceMapperStore[server]; !exists {
		mapper, err := resourcegraph.NewResourceMapper(restCfg)
		if err != nil {
			return fmt.Errorf("failed to create ResourceMapper: %w", err)
		}
		// Start CRD informer so add/update events invoke addToResourceList
		go mapper.StartInformer()
		rt.resourceMapperStore[server] = mapper
	}
	return nil
}

// restConfigFromCluster creates a rest.Config from a cluster
func restConfigFromCluster(c *v1alpha1.Cluster, kubeconfigPath string) (*rest.Config, error) {
	tls := rest.TLSClientConfig{
		Insecure:   c.Config.Insecure,
		ServerName: c.Config.ServerName,
		CertData:   c.Config.CertData,
		KeyData:    c.Config.KeyData,
		CAData:     c.Config.CAData,
	}

	var cfg *rest.Config

	// if the server is in-cluster, load the kubeconfig from the kubeconfig file or the default kubeconfig file
	if strings.Contains(c.Server, "kubernetes.default.svc") {
		fmt.Println("Detected in-cluster host (kubernetes.default.svc); loading kubeconfig...")
		localCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
		}
		cfg = localCfg
	} else {

		switch {
		case c.Config.AWSAuthConfig != nil:
			// EKS via argocd-k8s-auth (same contract as Argo CD)
			args := []string{"aws", "--cluster-name", c.Config.AWSAuthConfig.ClusterName}
			if c.Config.AWSAuthConfig.RoleARN != "" {
				args = append(args, "--role-arn", c.Config.AWSAuthConfig.RoleARN)
			}
			if c.Config.AWSAuthConfig.Profile != "" {
				args = append(args, "--profile", c.Config.AWSAuthConfig.Profile)
			}
			cfg = &rest.Config{
				Host:            c.Server,
				TLSClientConfig: tls,
				ExecProvider: &clientauthapi.ExecConfig{
					APIVersion:      "client.authentication.k8s.io/v1beta1",
					Command:         "argocd-k8s-auth",
					Args:            args,
					InteractiveMode: clientauthapi.NeverExecInteractiveMode,
				},
			}

		case c.Config.ExecProviderConfig != nil:
			// Generic exec provider (OIDC, SSO, etc.)
			var env []clientauthapi.ExecEnvVar
			for k, v := range c.Config.ExecProviderConfig.Env {
				env = append(env, clientauthapi.ExecEnvVar{Name: k, Value: v})
			}
			cfg = &rest.Config{
				Host:            c.Server,
				TLSClientConfig: tls,
				ExecProvider: &clientauthapi.ExecConfig{
					APIVersion:      c.Config.ExecProviderConfig.APIVersion,
					Command:         c.Config.ExecProviderConfig.Command,
					Args:            c.Config.ExecProviderConfig.Args,
					Env:             env,
					InstallHint:     c.Config.ExecProviderConfig.InstallHint,
					InteractiveMode: clientauthapi.NeverExecInteractiveMode,
				},
			}

		default:
			// Static auth (token or basic) and TLS
			cfg = &rest.Config{
				Host:            c.Server,
				Username:        c.Config.Username,
				Password:        c.Config.Password,
				BearerToken:     c.Config.BearerToken,
				TLSClientConfig: tls,
			}
		}
	}

	if c.Config.ProxyUrl != "" {
		u, err := v1alpha1.ParseProxyUrl(c.Config.ProxyUrl)
		if err != nil {
			return nil, fmt.Errorf("failed to parse proxy url: %w", err)
		}
		cfg.Proxy = http.ProxyURL(u)
	}
	// Apply Argo CD defaults
	cfg.DisableCompression = c.Config.DisableCompression
	cfg.Timeout = v1alpha1.K8sServerSideTimeout
	cfg.QPS = v1alpha1.K8sClientConfigQPS
	cfg.Burst = v1alpha1.K8sClientConfigBurst
	v1alpha1.SetK8SConfigDefaults(cfg)

	return cfg, nil
}

func getClusterServerByName(ctx context.Context, db db.ArgoDB, clusterName string) (string, error) {
	servers, err := db.GetClusterServersByName(ctx, clusterName)
	if err != nil {
		return "", fmt.Errorf("error getting cluster server by name %q: %w", clusterName, err)
	}
	if len(servers) > 1 {
		return "", fmt.Errorf("there are %d clusters with the same name: %v", len(servers), servers)
	} else if len(servers) == 0 {
		return "", fmt.Errorf("there are no clusters with this name: %s", clusterName)
	}
	return servers[0], nil
}

// mergeInto adds rel (parent -> children) into dst
func mergeInto(dst, rel map[string]*hashset.Set) {
	for p, set := range rel {
		if _, ok := dst[p]; !ok {
			dst[p] = hashset.New()
		}
		for _, v := range set.Values() {
			if s, ok := v.(string); ok {
				dst[p].Add(s)
			}
		}
	}
}

// tracker.go
// ensureSyncedSharedCacheOnHost checks cache; if miss, queries ONLY the given server.
func (rt *ResourceTracker) ensureSyncedSharedCacheOnHost(ctx context.Context, server string) {

	mapper, ok := rt.resourceMapperStore[server]
	if !ok || mapper == nil {
		// As a safety valve, you can return here OR (optionally) fall back to other mappers.
		fmt.Printf("warning: no mapper for host %s\n", server)
		return
	}
	fmt.Printf("ensureSyncedSharedCacheOnHost: querying relations host=%s\n", server)
	rel, err := mapper.GetResourcesRelation(ctx)
	if err != nil {
		fmt.Printf("warning: dynamic scan on %s failed: %v\n", server, err)
		return
	}

	rt.cacheMu.Lock()
	mergeInto(rt.sharedRelationsCache, rel)

	fmt.Printf("ensureSyncedSharedCacheOnHost: sharedRelationsCache=%v\n", rt.sharedRelationsCache)
	rt.cacheMu.Unlock()
}
