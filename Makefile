SHELL := /bin/bash
.SHELLFLAGS := -o pipefail -c
.SILENT:

INSTALLER_SIGN_IDENTITY := 21E03A44ACC2B48753BABB3DAE9B5F9A9CFF0480

lib:
	go run ./cmd/internal/build_libsharing

build_ios:
	xcodebuild build -skipMacroValidation -scheme OpenScreenSharing -configuration Debug -destination 'generic/platform=iOS' | xcbeautify | grep -A 10 -e "Build Succeeded" -e "BUILD FAILED" -e "❌"

DEV_DEVICE ?= 00008103-001E15990C0B001E

install_dev_ios:
	xcodebuild build -skipMacroValidation -allowProvisioningUpdates -scheme OpenScreenSharing -configuration Debug -destination 'id=$(DEV_DEVICE)' -derivedDataPath build/dd \
		DEVELOPMENT_TEAM=DH5UQL44AR PRODUCT_BUNDLE_IDENTIFIER=com.rockiestar.screensharing CODE_SIGN_STYLE=Automatic \
		ASSETCATALOG_COMPILER_APPICON_NAME=DevIcon INFOPLIST_KEY_CFBundleDisplayName="OSS Dev" EXCLUDED_SOURCE_FILE_NAMES=InfoPlist.xcstrings | xcbeautify | grep -A 10 -e "Build Succeeded" -e "BUILD FAILED" -e "❌"
	xcrun devicectl device install app --device $(DEV_DEVICE) "build/dd/Build/Products/Debug-iphoneos/Screen Sharing.app"

release_ios: lib archive_ios upload_ios

archive_ios:
	rm -rf build/OpenScreenSharing-iOS.xcarchive
	xcodebuild archive -skipMacroValidation $(XCODEBUILD_FLAGS) -scheme OpenScreenSharing -configuration Release -destination 'generic/platform=iOS' -archivePath build/OpenScreenSharing-iOS.xcarchive | xcbeautify

upload_ios:
	xcodebuild -exportArchive -archivePath build/OpenScreenSharing-iOS.xcarchive -exportOptionsPlist OpenScreenSharing/Upload.plist

export_ios:
	rm -rf build/OpenScreenSharing-iOS
	xcodebuild -exportArchive -archivePath build/OpenScreenSharing-iOS.xcarchive -exportOptionsPlist OpenScreenSharing/Export.plist -exportPath build/OpenScreenSharing-iOS

build_ios_ipa: lib archive_ios_ipa

archive_ios_ipa:
	rm -rf build/OpenScreenSharing-Sideload.xcarchive build/Payload build/OpenScreenSharing.ipa
	xcodebuild archive -skipMacroValidation $(XCODEBUILD_FLAGS) -scheme OpenScreenSharing -configuration Release -destination 'generic/platform=iOS' -archivePath build/OpenScreenSharing-Sideload.xcarchive \
		CODE_SIGNING_ALLOWED=NO SWIFT_ACTIVE_COMPILATION_CONDITIONS=SIDELOAD APP_INFOPLIST_FILE=OpenScreenSharing/Info-Server.plist EXCLUDED_SOURCE_FILE_NAMES='$$(SRCROOT)/OpenScreenSharing/Demo/*.jpg' | xcbeautify
	mkdir build/Payload
	cp -R build/OpenScreenSharing-Sideload.xcarchive/Products/Applications/Screen\ Sharing.app build/Payload/
	cd build && zip -qry OpenScreenSharing.ipa Payload
	rm -rf build/Payload

altstore_source:
	rm -rf build/altstore
	mkdir -p build/altstore
	"$$(xcode-select -p)/../Applications/Icon Composer.app/Contents/Executables/ictool" OpenScreenSharing/Resources/AppIcon.icon \
		--export-image --output-file build/altstore/icon.png --platform iOS --rendition Default --width 568 --height 568 --scale 1 > /dev/null
	# AltStore draws icons on a white background, clipped to a circle or a height/5 corner radius
	# (AltStore fafd76e, AltStore/Components/AppIconImageView.swift:46-65), so the transparent
	# corners and edge highlight of the exported squircle show up as a white border.
	sips --cropToHeightWidth 512 512 build/altstore/icon.png > /dev/null
	go run ./cmd/internal/build_altstore_source

.PHONY: screenshots
screenshots:
	go run ./cmd/internal/screenshots

build_macos:
	xcodebuild build -skipMacroValidation -scheme OpenScreenSharing -configuration Debug -destination 'generic/platform=macOS' | xcbeautify | grep -A 10 -e "Build Succeeded" -e "BUILD FAILED" -e "❌"

release_macos: lib archive_macos export_macos build_macos_pkg notarize_macos_pkg

archive_macos:
	rm -rf build/OpenScreenSharing-macOS.xcarchive
	xcodebuild archive -skipMacroValidation $(XCODEBUILD_FLAGS) -scheme OpenScreenSharing -configuration Release -destination 'generic/platform=macOS' -archivePath build/OpenScreenSharing-macOS.xcarchive | xcbeautify

export_macos:
	rm -rf build/OpenScreenSharing-macOS
	xcodebuild -exportArchive -archivePath build/OpenScreenSharing-macOS.xcarchive -exportOptionsPlist OpenScreenSharing/ExportDeveloperID.plist -exportPath build/OpenScreenSharing-macOS

build_macos_pkg:
	rm -rf build/pkgroot build/component.pkg build/OpenScreenSharing.pkg
	mkdir -p build/pkgroot
	ditto "build/OpenScreenSharing-macOS/Screen Sharing.app" "build/pkgroot/Screen Sharing.app"
	pkgbuild --root build/pkgroot \
		--component-plist OpenScreenSharing/component.plist \
		--identifier io.nekohasekai.screensharing \
		--install-location /Applications \
		--min-os-version 14.0 \
		--compression latest \
		build/component.pkg
	productbuild --distribution OpenScreenSharing/distribution.xml \
		--package-path build \
		--sign "$(INSTALLER_SIGN_IDENTITY)" \
		build/OpenScreenSharing.pkg
	rm -rf build/pkgroot build/component.pkg

notarize_macos_pkg:
	xcrun notarytool submit build/OpenScreenSharing.pkg --wait --keychain-profile "notarytool-password"
	xcrun stapler staple build/OpenScreenSharing.pkg

fmt:
	gofumpt -l -w cmd libsharing internal
	go run ./cmd/internal/format_xcstrings $$(find OpenScreenSharing -name '*.xcstrings')
