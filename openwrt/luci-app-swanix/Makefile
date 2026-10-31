include $(TOPDIR)/rules.mk

LUCI_TITLE:=LuCI support for swanix multi-WAN
LUCI_DEPENDS:=+luci-base +swanix-core
LUCI_PKGARCH:=all

PKG_LICENSE:=GPL-3.0-or-later
PKG_VERSION:=$(if $(SWANIX_VERSION),$(SWANIX_VERSION),0.0.0)

include $(TOPDIR)/feeds/luci/luci.mk


# call BuildPackage - OpenWrt buildroot signature
