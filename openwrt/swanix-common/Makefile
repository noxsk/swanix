include $(TOPDIR)/rules.mk

PKG_NAME:=swanix-common
PKG_VERSION:=$(SWANIX_VERSION)
PKG_RELEASE:=1

PKG_LICENSE:=GPL-3.0-or-later
PKG_LICENSE_FILES:=LICENSE

include $(INCLUDE_DIR)/package.mk

ifeq ($(SWANIX_VERSION),)
PKG_VERSION:=0.0.0
endif

define Package/swanix-common
  SECTION:=net
  CATEGORY:=Network
  TITLE:=swanix multi-WAN: configuration, init and helpers
  URL:=https://github.com/noxsk/swanix
  PKGARCH:=all
  DEPENDS:=+ip-full +nftables-json +firewall4 +kmod-nft-nat +ubus +jshn +ca-bundle
endef

define Package/swanix-common/description
  Shared files of the swanix multi-WAN controller: the default /etc/config/swanix
  (installed disabled, observe-only), the procd init script, the netifd hotplug
  hook and the rpcd plugin used by LuCI. Install exactly one of swanix-lite or
  swanix-full for the controller itself.
endef

define Package/swanix-common/conffiles
/etc/config/swanix
/etc/swanix/proxy.json
endef

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
endef

define Build/Configure
endef

define Build/Compile
endef

define Package/swanix-common/install
	$(INSTALL_DIR) $(1)/etc/config $(1)/etc/init.d $(1)/etc/hotplug.d/iface \
		$(1)/usr/libexec/rpcd $(1)/usr/share/swanix
	$(INSTALL_CONF) ./files/etc/config/swanix $(1)/etc/config/swanix
	$(INSTALL_BIN) ./files/etc/init.d/swanix $(1)/etc/init.d/swanix
	$(INSTALL_BIN) ./files/etc/hotplug.d/iface/90-swanix $(1)/etc/hotplug.d/iface/90-swanix
	$(INSTALL_BIN) ./files/usr/libexec/rpcd/swanix $(1)/usr/libexec/rpcd/swanix
	$(INSTALL_DATA) ./files/usr/share/swanix/proxy.json.example $(1)/usr/share/swanix/proxy.json.example
endef

# The package never enables the service or touches mwan3. Leaving the
# installed-in-an-offline-root case (SDK image builds) alone is deliberate:
# nothing here talks to the host network.
define Package/swanix-common/postinst
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
exit 0
endef

define Package/swanix-common/prerm
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
# removal withdraws only the objects swanix owns
[ -x /usr/bin/swanix ] && /usr/bin/swanix remove >/dev/null 2>&1
/etc/init.d/swanix stop >/dev/null 2>&1
exit 0
endef

$(eval $(call BuildPackage,swanix-common))
