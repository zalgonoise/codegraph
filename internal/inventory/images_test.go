package inventory

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// The images a repository builds, from each kind of file, with the variables
// those files assign expanded; and those it only uses.
func TestImages(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "shop.git")
	write(t, repo, map[string]string{
		// A pipeline building two images in turn, the registry and branch unknown.
		"bitbucket-pipelines.yml": `image: atlassian/default-image:4
pipelines:
  default:
    - step:
        script:
          - TAG=$(echo ${BITBUCKET_BRANCH:-$BITBUCKET_TAG} | tr '[:upper:]' '[:lower:]')
          - PROJECT=shop
          - AWS_IMG=$AWS_REGISTRY_URL/team-${PROJECT}:release-$TAG
          - docker build -t $TAG -f Dockerfile_shop .
          - docker tag $TAG ${AWS_IMG}
          - docker push ${AWS_IMG}
    - step:
        script:
          - PROJECT=worker
          - AWS_IMG=$AWS_REGISTRY_URL/team-${PROJECT}:release-$TAG
          - docker build -t $TAG -f Dockerfile_worker . && docker tag $TAG ${AWS_IMG} && docker push ${AWS_IMG}
`,
		// A job calling the organisation's reusable workflow, and a step pushing with build-push-action.
		".github/workflows/ci.yaml": `env:
  REGISTRY: ghcr.io/example
jobs:
  shop:
    uses: example/.github/.github/workflows/build.yaml@v2
    with:
      IMAGE_PREFIX: team
      PROJECT: shop-api
      dockerfile: Dockerfile_github
  docs:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/build-push-action@v6
        with:
          file: docs/Dockerfile
          tags: |
            ${{ env.REGISTRY }}/shop-docs:latest
            ${{ env.REGISTRY }}/shop-docs:${{ github.sha }}
`,
		"Makefile":    "IMAGE ?= registry.example.com/team/shop-cli\nVERSION := 1.2\n\nimage:\n\tdocker build -t $(IMAGE):$(VERSION) .\n\tdocker push $(IMAGE):$(VERSION)\n\tdocker push docker/$*\n",
		"Jenkinsfile": "node {\n  def img = docker.build(\"team/shop-jobs:${env.BUILD_ID}\")\n}\n",
		"docker-compose.yml": `services:
  api:
    build: {context: ., dockerfile: Dockerfile_dev}
    image: shop-dev
  db:
    image: postgres:16
`,
		"Dockerfile":                    "ARG GO=1.27\nFROM golang:${GO} AS build\nFROM build AS test\nFROM gcr.io/distroless/static:nonroot\n",
		"deploy/chart/values.yaml":      "image:\n  registry: docker.io\n  repository: bitnami/redis\n  tag: 7.2\nflag:\n  image: true\n",
		"deploy/chart/templates/x.yaml": "image: {{ .Values.image.repository }}\n",
		"k8s/cron.yaml":                 "spec:\n  containers:\n    - image: busybox:1.36\n",
	})
	inv := Scan([]string{repo}, Conventions{WorkflowImage: "{IMAGE_PREFIX}-{PROJECT}"})
	r := inv.Repos[0]
	var got []string
	for _, im := range r.Images {
		got = append(got, fmt.Sprintf("%s:%s %s %s %s", im.Name, im.Tag, im.Via, im.Dockerfile, im.File))
	}
	want := []string{
		"example/shop-docs:latest build-push-action docs/Dockerfile .github/workflows/ci.yaml:16", // ghcr.io/example/…
		"shop-dev: compose Dockerfile_dev docker-compose.yml:4",
		"team-shop:release-* docker push Dockerfile_shop bitbucket-pipelines.yml:11",
		"team-shop-api: workflow inputs Dockerfile_github .github/workflows/ci.yaml:4",
		"team-worker:release-* docker push Dockerfile_worker bitbucket-pipelines.yml:16",
		"team/shop-cli:1.2 docker push  Makefile:6",
		"team/shop-jobs:* Jenkins  Jenkinsfile:2",
	}
	if !slices.Equal(got, want) {
		t.Errorf("images:\n got %q\nwant %q", got, want)
	}
	var refs []string
	for _, im := range r.References {
		refs = append(refs, fmt.Sprintf("%s:%s %s", im.Name, im.Tag, im.Via))
	}
	wantRefs := []string{
		"atlassian/default-image:4 CI", "bitnami/redis:7.2 helm values", "busybox:1.36 manifest",
		"distroless/static:nonroot FROM", "golang:1.27 FROM", "postgres:16 compose",
	}
	if !slices.Equal(refs, wantRefs) {
		t.Errorf("references:\n got %q\nwant %q", refs, wantRefs)
	}

	// Builders and referrers, by reference or name, whole or by last segment.
	for _, c := range []struct{ ref, want string }{
		{"123.dkr.ecr.example.com/team-shop:release-main", "shop"},
		{"proxy/acme/team-worker:release-v1", "shop"},
		{"team/shop-cli", "shop"},
		{"nobody/else", ""},
	} {
		var who []string
		for _, b := range inv.Builders(c.ref) {
			who = append(who, b.Repo)
		}
		if got := fmt.Sprint(who); (c.want == "" && len(who) > 0) || (c.want != "" && got != "["+c.want+"]") {
			t.Errorf("Builders(%q) = %v, want %q", c.ref, who, c.want)
		}
	}
	if b := inv.Referrers("docker-hub/bitnami/redis:7.4"); len(b) != 1 || b[0].Image.File != "deploy/chart/values.yaml:3" {
		t.Errorf("Referrers = %+v", b)
	}
}

func TestImageOf(t *testing.T) {
	for ref, want := range map[string]string{
		"registry.example.com:5000/team/app:v1": "team/app", "*/team-app:release-*": "team-app", "app@sha256:abc": "app",
		"localhost/app": "app", "Team/App": "team/app", "*": "", "team/*": "", "-t": "", "a b": "",
	} {
		got, ok := imageOf(ref)
		if want == "" && ok || want != "" && (!ok || got.Name != want) {
			t.Errorf("imageOf(%q) = %q (%v), want %q", ref, got.Name, ok, want)
		}
	}
}
