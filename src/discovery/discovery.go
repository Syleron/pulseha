package discovery

type Service struct {}

type Registry struct {}

func (r *Registry) Register() {}

func (r *Registry) Unregister() {}

func (r *Registry) Connect() {}

func New() {
	//r := new(Registry)
}
