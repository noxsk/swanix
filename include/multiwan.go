package include

import (
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/service/multiwan"
)

func registerMultiWANService(registry *service.Registry) {
	multiwan.RegisterService(registry)
}
