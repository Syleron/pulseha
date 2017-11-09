def repoName = 'PulseHA'

def fileRef

stage('Build') {
        node {
                checkout scm
                def root = tool name: 'go 1.9', type: 'go'
                // Export environment variables pointing to the directory where Go was installed
                withEnv(["GOROOT=${root}","PATH+GO=${root}/bin","GOPATH=/var/lib/jenkins/go","GOBIN=/var/lib/jenkins/go/bin"]) {
                         sh 'make'
                         sh 'make cli'
                }
        }
}
