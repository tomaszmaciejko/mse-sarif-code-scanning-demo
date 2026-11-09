android {
    flavorDimensions += "compatibility"
    productFlavors {
        create("legacy") {
            dimension = "compatibility"
            targetSdk = 27
        }
        create("modern") {
            dimension = "compatibility"
            targetSdk = libs.versions.android.target.sdk.get().toInt()
        }
    }
}
