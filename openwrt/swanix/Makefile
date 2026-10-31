include $(TOPDIR)/rules.mk

# swanix: multi-WAN controller for OpenWrt, built from the sing-box fork in this repository.
# Two variants share one source tree and one /usr/bin/swanix:
#   lite  pure-Go MultiWAN controller, no proxy code linked (build tag mw_lite)
#   full  the reviewed upstream sing-box feature set plus MultiWAN
#
# SWANIX_SRC is the swanix source tree (the repository this feed was linked
# from, or an unpacked release tarball); it is only read when compiling from source.

PKG_NAME:=swanix
PKG_VERSION:=$(if $(SWANIX_VERSION),$(SWANIX_VERSION),0.0.0)
PKG_RELEASE:=1

PKG_LICENSE:=GPL-3.0-or-later
PKG_LICENSE_FILES:=LICENSE

SWANIX_SRC?=
SWANIX_COMMIT?=unknown
SWANIX_OPENWRT?=
SWANIX_VERIFIED?=

# SWANIX_PREBUILT points at a directory holding swanix-lite / swanix-full built
# with the locked toolchain (ci/build/binary.sh); the SDK then only packages.
# Without it the package compiles from source with the buildroot's Go.
ifeq ($(SWANIX_PREBUILT),)
PKG_BUILD_DEPENDS:=golang/host
endif
PKG_BUILD_PARALLEL:=1
PKG_BUILD_FLAGS:=no-mips16

GO_PKG:=github.com/sagernet/sing-box
GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/swanix

BI:=$(GO_PKG)/experimental/multiwan/buildinfo
GO_PKG_LDFLAGS_X:= \
	$(BI).Version=$(PKG_VERSION) \
	$(BI).Commit=$(SWANIX_COMMIT) \
	$(BI).OpenWrt=$(SWANIX_OPENWRT) \
	$(BI).Verified=$(SWANIX_VERIFIED)

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Package/swanix/default
  SECTION:=net
  CATEGORY:=Network
  URL:=https://github.com/noxsk/swanix
  DEPENDS:=$(GO_ARCH_DEPENDS) +swanix-common
  PROVIDES:=swanix-core
endef

define Package/swanix-lite
  $(call Package/swanix/default)
  TITLE:=swanix multi-WAN controller (Lite)
  VARIANT:=lite
  # Declared on one side only: a CONFLICTS on both makes Kconfig report a
  # recursive dependency. opkg enforces it in either direction; apk packages carry no
  # Conflicts field, but both cores provide swanix-core=<version>, which apk refuses to
  # install side by side.
  CONFLICTS:=swanix-full
endef

define Package/swanix-lite/description
  Multi-WAN connection balancing, health probing, policy routing and the apply/confirm
  rollback flow. The proxy stack is not linked into this build.
endef

define Package/swanix-full
  $(call Package/swanix/default)
  TITLE:=swanix multi-WAN controller (Full, with proxy)
  VARIANT:=full
  DEPENDS+=+kmod-tun +kmod-inet-diag
endef

define Package/swanix-full/description
  Everything in swanix-lite plus the sing-box proxy runtime of the locked upstream
  version, with a wan_policy dial option that picks a WAN per outgoing connection.
endef

# ---- build configuration -----------------------------------------------------
# Only needed when compiling from source; with SWANIX_PREBUILT the SDK just
# packages a binary built by ci/build/binary.sh. Skipped while the buildroot
# merely dumps package metadata (DUMP is set then).

ifeq ($(DUMP)$(SWANIX_PREBUILT),)
  ifeq ($(SWANIX_SRC),)
    $(error SWANIX_SRC must point at the swanix source tree (or set SWANIX_PREBUILT))
  endif
  ifeq ($(BUILD_VARIANT),lite)
    GO_PKG_TAGS:=mw_lite
    GO_PKG_LDFLAGS:=-buildid=
    GO_PKG_BUILD_VARS+= CGO_ENABLED=0
  else
    # The tag list is the reviewed features lock of the locked upstream tag: it is
    # never trimmed here. A missing capability stops the build instead of
    # silently producing a smaller "Full".
    SWANIX_FULL_TAGS:=$(shell cat $(SWANIX_SRC)/ci/locks/full-tags.txt 2>/dev/null)
    ifeq ($(strip $(SWANIX_FULL_TAGS)),)
      $(error ci/locks/full-tags.txt is missing or empty: refusing to build an unreviewed Full)
    endif
    ifneq ($(findstring with_naive_outbound,$(SWANIX_FULL_TAGS)),)
      ifeq ($(SWANIX_CRONET_ENV),)
        $(error Full needs the cronet toolchain for with_naive_outbound (see ci/build/cronet.sh); set SWANIX_CRONET_ENV, or build the binary with ci/build/binary.sh and pass SWANIX_PREBUILT)
      endif
    endif
    GO_PKG_TAGS:=$(subst $(space),$(comma),$(strip $(SWANIX_FULL_TAGS) with_musl))
    # upstream's required linker flags (release/LDFLAGS) are kept verbatim
    GO_PKG_LDFLAGS:=$(shell cat $(SWANIX_SRC)/release/LDFLAGS 2>/dev/null) -buildid=
  endif
endif

ifeq ($(SWANIX_PREBUILT),)
define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) $(SWANIX_SRC)/. $(PKG_BUILD_DIR)/
	rm -rf $(PKG_BUILD_DIR)/.git
endef
else
define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
endef
define Build/Configure
endef
define Build/InstallDev
endef
define Build/Compile
	$(INSTALL_DIR) $(GO_PKG_BUILD_BIN_DIR)
	$(INSTALL_BIN) $(SWANIX_PREBUILT)/swanix-$(BUILD_VARIANT) $(GO_PKG_BUILD_BIN_DIR)/swanix
endef
endif

# The package that owns /usr/bin/swanix must withdraw the kernel objects while the binary still
# exists; swanix-common's own prerm may run after the binary is gone. An upgrade runs this too
# (the package manager gives scripts no reliable "upgrade" hint), so postinst starts the service
# again when it is enabled; a first install is never enabled and starts nothing.
define Package/swanix-lite/prerm
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
/etc/init.d/swanix stop >/dev/null 2>&1
[ -x /usr/bin/swanix ] && /usr/bin/swanix remove >/dev/null 2>&1
exit 0
endef
Package/swanix-full/prerm=$(Package/swanix-lite/prerm)

define Package/swanix-lite/postinst
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
/etc/init.d/swanix enabled && /etc/init.d/swanix restart >/dev/null 2>&1
exit 0
endef
Package/swanix-full/postinst=$(Package/swanix-lite/postinst)

define Package/swanix-lite/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/swanix $(1)/usr/bin/swanix
endef

define Package/swanix-full/install
	$(INSTALL_DIR) $(1)/usr/bin $(1)/usr/libexec/swanix
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/swanix $(1)/usr/bin/swanix
	$(INSTALL_BIN) ./files/full-marker $(1)/usr/libexec/swanix/full
endef

$(eval $(call BuildPackage,swanix-lite))
$(eval $(call BuildPackage,swanix-full))
