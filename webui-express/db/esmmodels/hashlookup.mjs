import { Model, DataTypes } from "../../src/adapter.js";
import sequelize from "../config/sequelize.mjs";

export class HashLookup extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
        // define association here
    }
}

HashLookup.init(
    {
        txn_uuid: DataTypes.STRING,
        md5: DataTypes.STRING,
        contentType: DataTypes.STRING,
        filename: DataTypes.STRING,
        size: DataTypes.INTEGER,
        action: DataTypes.STRING,
    },
    {
        sequelize,
        modelName: "HashLookup",
    }
);

export default HashLookup;
