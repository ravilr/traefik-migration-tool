// Package ingress convert Ingress to IngressRoute.
package ingress

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/traefik/traefik/v2/pkg/provider/kubernetes/crd/traefik/v1alpha1"
	extensions "k8s.io/api/extensions/v1beta1"
	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

const separator = "---"

const groupSuffix = "/v1alpha1"

const middlewareSuffix = "kubernetescrd"

const sslMiddlewareRef = "ssl-redirect@file"

const (
	ruleTypePath             = "Path"
	ruleTypePathPrefix       = "PathPrefix"
	ruleTypePathStrip        = "PathStrip"
	ruleTypePathPrefixStrip  = "PathPrefixStrip"
	ruleTypeAddPrefix        = "AddPrefix"
	ruleTypeReplacePath      = "ReplacePath"
	ruleTypeReplacePathRegex = "ReplacePathRegex"
)

// Convert converts all ingress in a src into a dstDir.
func Convert(src, dstDir string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		filename := info.Name()
		srcPath := filepath.Dir(src)
		return convertFile(srcPath, dstDir, filename)
	}

	dir := info.Name()
	infos, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, info := range infos {
		newSrc := filepath.Join(src, info.Name())
		newDst := filepath.Join(dstDir, dir)
		err := Convert(newSrc, newDst)
		if err != nil {
			return err
		}
	}
	return nil
}

func convertFile(srcDir, dstDir, filename string) error {
	content, err := expandFileContent(filepath.Join(srcDir, filename))
	if err != nil {
		return err
	}

	err = os.MkdirAll(dstDir, 0755)
	if err != nil {
		return err
	}

	parts := strings.Split(string(content), separator)
	var fragments []string
	for _, part := range parts {
		if part == "\n" || part == "" {
			continue
		}

		unstruct, err := createUnstructured([]byte(part))
		if err != nil {
			return err
		}

		if unstruct.IsList() {
			fragments = append(fragments, part)
			continue
		}

		object, err := parseYaml([]byte(part))
		if err != nil {
			log.Printf("err while reading yaml: %v\n", err)
			fragments = append(fragments, part)
			continue
		}

		var ingress *networking.Ingress
		switch obj := object.(type) {
		case *extensions.Ingress:
			ingress, err = extensionsToNetworking(obj)
			if err != nil {
				return err
			}
		case *networking.Ingress:
			ingress = obj
		default:
			log.Printf("the object is skipped because is not an Ingress: %T\n", object)
			fragments = append(fragments, part)
			continue
		}

		newIngress, objects := convertIngress(ingress)
		yml, err := encodeYaml(newIngress.DeepCopyObject(), networking.SchemeGroupVersion.Group+"/"+networking.SchemeGroupVersion.Version)
		if err != nil {
			return err
		}
		fragments = append(fragments, yml)
		for _, object := range objects {
			yml, err := encodeYaml(object, v1alpha1.GroupName+groupSuffix)
			if err != nil {
				return err
			}
			fragments = append(fragments, yml)
		}
	}

	return os.WriteFile(filepath.Join(dstDir, filename), []byte(strings.Join(fragments, separator+"\n")), 0666)
}

func expandFileContent(filePath string) ([]byte, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	parts := strings.Split(string(content), separator)
	var fragments []string
	for _, part := range parts {
		if part == "\n" || part == "" {
			continue
		}

		listObj, err := createUnstructured(content)
		if err != nil {
			return nil, err
		}

		if !listObj.IsList() {
			fragments = append(fragments, part)
			continue
		}

		items, _, err := unstructured.NestedSlice(listObj.Object, "items")
		if err != nil {
			return nil, err
		}

		toKeep, toConvert := extractItems(items)

		if len(items) == len(toKeep) {
			fragments = append(fragments, part)
			continue
		}

		if len(toKeep) > 0 {
			newObj := listObj.DeepCopy()

			err = unstructured.SetNestedSlice(newObj.Object, toKeep, "items")
			if err != nil {
				return nil, err
			}

			m, err := yaml.Marshal(newObj)
			if err != nil {
				return nil, err
			}

			fragments = append(fragments, string(m))
		}

		for _, elt := range toConvert {
			m, err := yaml.Marshal(elt.Object)
			if err != nil {
				return nil, err
			}
			fragments = append(fragments, string(m))
		}
	}

	return []byte(strings.Join(fragments, separator+"\n")), nil
}

func createUnstructured(content []byte) (*unstructured.Unstructured, error) {
	listObj := &unstructured.Unstructured{Object: map[string]interface{}{}}

	if err := yaml.Unmarshal(content, &listObj.Object); err != nil {
		return nil, fmt.Errorf("error decoding YAML: %w\noriginal YAML: %s", err, string(content))
	}

	return listObj, nil
}

func extractItems(items []interface{}) ([]interface{}, []unstructured.Unstructured) {
	var toKeep []interface{}
	var toConvert []unstructured.Unstructured

	for _, elt := range items {
		obj := unstructured.Unstructured{Object: elt.(map[string]interface{})}
		if (obj.GetAPIVersion() == "extensions/v1beta1" || obj.GetAPIVersion() == "networking.k8s.io/v1") && obj.GetKind() == "Ingress" {
			toConvert = append(toConvert, obj)
		} else {
			toKeep = append(toKeep, elt)
		}
	}

	return toKeep, toConvert
}

// convertIngress converts an *networking.Ingress to a slice of runtime.Object (Ingress and Middlewares).
func convertIngress(ingress *networking.Ingress) (*networking.Ingress, []runtime.Object) {
	logUnsupported(ingress)

	var middlewareNames []string

	newIngress := ingress.DeepCopy()

	// return early, if the ingressClass is not 'traefik*'
	ingressClass := getStringValue(ingress.GetAnnotations(), annotationKubernetesIngressClass, "")
	if len(ingressClass) > 0 && !strings.Contains(ingressClass, "traefik") {
		return newIngress, nil
	}

	entryPoints := getStringValue(ingress.GetAnnotations(), annotationKubernetesFrontendEntryPoints, "")
	if entryPoints != "" {
		newIngress.GetAnnotations()["traefik.ingress.kubernetes.io/router.entrypoints"] = entryPoints
	}

	sslRedirect := getStringValue(ingress.GetAnnotations(), annotationKubernetesSSLRedirect, "")
	if sslRedirect != "" {
		middlewareNames = append(middlewareNames, sslMiddlewareRef)
	}

	var middlewares []*v1alpha1.Middleware

	// Auth middleware
	auth := getAuthMiddleware(ingress)
	if auth != nil {
		middlewares = append(middlewares, auth)
		middlewareNames = append(middlewareNames, fmt.Sprintf("%s-%s@%s", auth.GetNamespace(), auth.GetName(), middlewareSuffix))
	}

	// Headers middleware
	headers := getHeadersMiddleware(ingress)
	if headers != nil {
		middlewares = append(middlewares, headers)
		middlewareNames = append(middlewareNames, fmt.Sprintf("%s-%s@%s", headers.GetNamespace(), headers.GetName(), middlewareSuffix))
	}

	// Whitelist middleware
	whiteList := getWhiteList(ingress)
	if whiteList != nil {
		middlewares = append(middlewares, whiteList)
		middlewareNames = append(middlewareNames, fmt.Sprintf("%s-%s@%s", whiteList.GetNamespace(), whiteList.GetName(), middlewareSuffix))
	}

	requestModifier := getStringValue(ingress.GetAnnotations(), annotationKubernetesRequestModifier, "")
	if requestModifier != "" {
		middleware, err := parseRequestModifier(ingress.GetNamespace(), requestModifier, ingress.GetName())
		if err != nil {
			log.Printf("Invalid %s: %v\n", annotationKubernetesRequestModifier, err)
		} else {
			middlewares = append(middlewares, middleware)
			middlewareNames = append(middlewareNames, fmt.Sprintf("%s-%s@%s", middleware.GetNamespace(), middleware.GetName(), middlewareSuffix))
		}
	}

	if appRoot := getStringValue(ingress.GetAnnotations(), annotationKubernetesAppRoot, ""); appRoot == "" {
		redirect := getFrontendRedirect(ingress.GetNamespace(), ingress.GetName(), ingress.GetAnnotations())
		if redirect != nil {
			middlewares = append(middlewares, redirect)
			middlewareNames = append(middlewareNames, fmt.Sprintf("%s-%s@%s", redirect.GetNamespace(), redirect.GetName(), middlewareSuffix))
		}
	}

	ruleType, stripPrefix, err := extractRuleType(ingress.GetAnnotations())
	if err != nil {
		log.Println(err)
		return nil, nil
	}

	for _, rule := range ingress.Spec.Rules {
		for _, path := range rule.HTTP.Paths {
			if len(path.Path) > 0 {
				if stripPrefix {
					mi := getStripPrefix(path, rule.Host+path.Path, ingress.GetNamespace())
					middlewares = append(middlewares, mi)
					middlewareNames = append(middlewareNames, fmt.Sprintf("%s-%s@%s", mi.GetNamespace(), mi.GetName(), middlewareSuffix))
				}

				rewriteTarget := getStringValue(ingress.GetAnnotations(), annotationKubernetesRewriteTarget, "")
				if rewriteTarget != "" {
					if ruleType == ruleTypeReplacePath {
						log.Printf("rewrite-target must not be used together with annotation %q\n", annotationKubernetesRuleType)
						return nil, nil
					}

					mi := getReplacePathRegex(rule, path, ingress.GetNamespace(), rewriteTarget)
					middlewares = append(middlewares, mi)
					middlewareNames = append(middlewareNames, fmt.Sprintf("%s-%s@%s", mi.GetNamespace(), mi.GetName(), middlewareSuffix))
				}
			}
			redirect := getFrontendRedirectAppRoot(ingress.GetNamespace(), ingress.GetName(), ingress.GetAnnotations(), rule.Host+path.Path, path.Path)
			if redirect != nil {
				middlewares = append(middlewares, redirect)
				middlewareNames = append(middlewareNames, fmt.Sprintf("%s-%s@%s", redirect.GetNamespace(), redirect.GetName(), middlewareSuffix))
			}

		}
	}

	if len(middlewareNames) > 0 {
		newIngress.GetAnnotations()["traefik.ingress.kubernetes.io/router.middlewares"] = strings.Join(middlewareNames, ",")
	}

	sort.Slice(middlewares, func(i, j int) bool { return middlewares[i].Name < middlewares[j].Name })

	objects := []runtime.Object{}
	for _, middleware := range middlewares {
		objects = append(objects, middleware)
	}

	return newIngress, objects
}

func extractRuleType(annotations map[string]string) (string, bool, error) {
	var stripPrefix bool
	ruleType := getStringValue(annotations, annotationKubernetesRuleType, ruleTypePathPrefix)

	switch ruleType {
	case ruleTypePath, ruleTypePathPrefix:
	case ruleTypePathStrip:
		ruleType = ruleTypePath
		stripPrefix = true
	case ruleTypePathPrefixStrip:
		ruleType = ruleTypePathPrefix
		stripPrefix = true
	case ruleTypeReplacePath:
		log.Printf("Using %s as %s will be deprecated in the future. Please use the %s annotation instead\n", ruleType, annotationKubernetesRuleType, annotationKubernetesRequestModifier)
	default:
		return "", false, fmt.Errorf("cannot use non-matcher rule: %q", ruleType)
	}

	return ruleType, stripPrefix, nil
}

func logUnsupported(ingress *networking.Ingress) {
	unsupportedAnnotations := map[string]string{
		annotationKubernetesErrorPages:                      "See https://docs.traefik.io/middlewares/errorpages/",
		annotationKubernetesBuffering:                       "See https://docs.traefik.io/middlewares/buffering/",
		annotationKubernetesCircuitBreakerExpression:        "See https://docs.traefik.io/middlewares/circuitbreaker/",
		annotationKubernetesMaxConnAmount:                   "See https://docs.traefik.io/middlewares/inflightreq/",
		annotationKubernetesMaxConnExtractorFunc:            "See https://docs.traefik.io/middlewares/inflightreq/",
		annotationKubernetesResponseForwardingFlushInterval: "See https://docs.traefik.io/providers/kubernetes-crd/",
		annotationKubernetesLoadBalancerMethod:              "See https://docs.traefik.io/providers/kubernetes-crd/",
		annotationKubernetesAuthRealm:                       "See https://docs.traefik.io/middlewares/basicauth/",
		annotationKubernetesServiceWeights:                  "See https://docs.traefik.io/providers/kubernetes-crd/",
		annotationKubernetesProtocol:                        "set traefik.ingress.kubernetes.io/service.serversscheme on Service resource",
		annotationKubernetesPreserveHost:                    "set traefik.ingress.kubernetes.io/service.passhostheader on Service resource",
	}

	for annot, msg := range unsupportedAnnotations {
		if getStringValue(ingress.GetAnnotations(), annot, "") != "" {
			fmt.Printf("%s/%s: The annotation %s on Ingress must be converted manually. %s\n", ingress.GetNamespace(), ingress.GetName(), annot, msg)
		}
	}
}

// https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names
func normalizeObjectName(name string) string {
	fn := func(c rune) bool {
		return !unicode.IsLetter(c) && !unicode.IsNumber(c) && c != '-'
	}

	return strings.Join(strings.FieldsFunc(name, fn), "-")
}
